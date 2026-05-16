#!/usr/bin/env python3
"""LLM Proxy 并发吞吐基准测试 — see doc/proxy-benchmarking.md for full guide.

Required (env or flags):
    POLAR_PROXY_TOKEN      bearer plaintext (polar_proxy_…)
    POLAR_PROXY_BASE_URL   e.g. https://zen.4950.store/api/proxy/v1
    --model NAME           model the client should ask for

Quick:
    export POLAR_PROXY_TOKEN=polar_proxy_xxxxxxxx...
    export POLAR_PROXY_BASE_URL=https://zen.4950.store/api/proxy/v1
    python3 proxy-bench.py --model Qwen3_Standard

Sweeps:
    python3 proxy-bench.py --model X --concurrency-sweep 1,2,4,8,16
    python3 proxy-bench.py --model X --mode stream            # only streaming
    python3 proxy-bench.py --model X --requests-per-level 30  # more samples

Dep: pip install httpx  (one-liner — no project install needed)
"""
import argparse
import asyncio
import json
import os
import statistics
import sys
import time
from dataclasses import dataclass
from typing import Optional

import httpx

# Env-var defaults; CLI flags override. No live token is shipped in repo.
DEFAULT_BASE_URL = os.environ.get("POLAR_PROXY_BASE_URL", "")
DEFAULT_TOKEN = os.environ.get("POLAR_PROXY_TOKEN", "")
DEFAULT_MODEL = os.environ.get("POLAR_PROXY_MODEL", "")

# Prompt 池 — 长度 + 难度做了梯度，避免 KV cache 命中扭曲吞吐数据。
PROMPTS = [
    "Write a Python function that returns the nth Fibonacci number using memoization. Include type hints and a docstring.",
    "Explain the CAP theorem in about 120 words, with a concrete example for each of the three guarantees.",
    "Implement an in-place quicksort in Python and walk through the partition step.",
    "List 5 substantive differences between PostgreSQL and MySQL for OLTP workloads.",
    "Write a SQL query to find the top 5 highest-paid employees per department; assume an employees(id, dept_id, salary) table.",
    "Compare gRPC unary vs streaming RPCs: when do you pick each? Two paragraphs.",
    "Describe how Raft handles leader failure end-to-end — election timeout, vote RPCs, log catch-up.",
    "Write a short Go function that reads a file line-by-line and returns the longest line. Handle empty file gracefully.",
]


@dataclass
class Result:
    ok: bool
    e2e_ms: float
    ttft_ms: Optional[float] = None  # streaming only
    prompt_tokens: int = 0
    completion_tokens: int = 0
    status: int = 0
    error: str = ""


async def fire_nonstream(client: httpx.AsyncClient, base_url: str, token: str, model: str, prompt: str) -> Result:
    t0 = time.perf_counter()
    try:
        r = await client.post(
            f"{base_url}/chat/completions",
            headers={"Authorization": f"Bearer {token}"},
            json={
                "model": model,
                "messages": [{"role": "user", "content": prompt}],
            },
            timeout=180,
        )
        e2e = (time.perf_counter() - t0) * 1000
        if r.status_code != 200:
            body = r.text[:120].replace("\n", " ")
            return Result(ok=False, e2e_ms=e2e, status=r.status_code, error=body)
        data = r.json()
        usage = data.get("usage") or {}
        return Result(
            ok=True, e2e_ms=e2e, status=200,
            prompt_tokens=int(usage.get("prompt_tokens") or 0),
            completion_tokens=int(usage.get("completion_tokens") or 0),
        )
    except Exception as e:
        return Result(ok=False, e2e_ms=(time.perf_counter()-t0)*1000, error=type(e).__name__ + ": " + str(e)[:120])


async def fire_stream(client: httpx.AsyncClient, base_url: str, token: str, model: str, prompt: str) -> Result:
    t0 = time.perf_counter()
    ttft: Optional[float] = None
    prompt_tokens = 0
    completion_tokens = 0
    try:
        async with client.stream(
            "POST", f"{base_url}/chat/completions",
            headers={"Authorization": f"Bearer {token}"},
            json={
                "model": model,
                "stream": True,
                "messages": [{"role": "user", "content": prompt}],
            },
            timeout=180,
        ) as r:
            if r.status_code != 200:
                body = (await r.aread())[:120].decode(errors="replace").replace("\n", " ")
                return Result(ok=False, e2e_ms=(time.perf_counter()-t0)*1000, status=r.status_code, error=body)
            inline_err: Optional[str] = None
            async for line in r.aiter_lines():
                if not line.startswith("data: "):
                    continue
                payload = line[6:].strip()
                if payload == "[DONE]" or not payload:
                    continue
                try:
                    chunk = json.loads(payload)
                except json.JSONDecodeError:
                    continue
                # Dock emits OpenAI-shaped error frames mid-stream when
                # upstream fails. Treat those as a failure result even
                # though HTTP was 200.
                if isinstance(chunk.get("error"), dict):
                    inline_err = str(chunk["error"].get("message") or chunk["error"])[:200]
                    continue
                if ttft is None:
                    choices = chunk.get("choices") or []
                    if choices:
                        delta = choices[0].get("delta") or {}
                        if delta.get("content"):
                            ttft = (time.perf_counter() - t0) * 1000
                usage = chunk.get("usage")
                if usage:
                    prompt_tokens = int(usage.get("prompt_tokens") or prompt_tokens)
                    completion_tokens = int(usage.get("completion_tokens") or completion_tokens)
        e2e = (time.perf_counter() - t0) * 1000
        if inline_err is not None:
            return Result(ok=False, e2e_ms=e2e, status=200, error="inline: " + inline_err)
        # An empty SSE that finished cleanly but yielded 0 completion
        # tokens is also a soft failure — boss-report should not count
        # it as throughput.
        if completion_tokens == 0 and ttft is None:
            return Result(ok=False, e2e_ms=e2e, status=200, error="empty stream (no content + no usage)")
        return Result(
            ok=True, e2e_ms=e2e, status=200, ttft_ms=ttft,
            prompt_tokens=prompt_tokens, completion_tokens=completion_tokens,
        )
    except Exception as e:
        return Result(ok=False, e2e_ms=(time.perf_counter()-t0)*1000, error=type(e).__name__ + ": " + str(e)[:120])


async def run_batch(concurrency: int, total: int, mode: str, base_url: str, token: str, model: str):
    sem = asyncio.Semaphore(concurrency)
    results = []
    t_start = time.perf_counter()
    limits = httpx.Limits(max_connections=max(concurrency * 2, 16), max_keepalive_connections=concurrency)
    # trust_env=False so we ignore HTTP(S)_PROXY in the env — the dev
    # box's CONNECT tunnel doesn't know about zen.4950.store and 502s.
    async with httpx.AsyncClient(http2=False, limits=limits, verify=True, trust_env=False) as client:
        async def worker(i: int) -> Result:
            async with sem:
                prompt = PROMPTS[i % len(PROMPTS)]
                if mode == "stream":
                    return await fire_stream(client, base_url, token, model, prompt)
                return await fire_nonstream(client, base_url, token, model, prompt)

        tasks = [asyncio.create_task(worker(i)) for i in range(total)]
        done = 0
        for fut in asyncio.as_completed(tasks):
            r = await fut
            results.append(r)
            done += 1
            extra = f" ttft={r.ttft_ms:>5.0f}ms" if r.ttft_ms is not None else " " * 13
            tag = "ok " if r.ok else f"ERR{r.status:<3}"
            errtail = f" :: {r.error}" if not r.ok else ""
            print(f"  [{done:>3}/{total}] {tag} e2e={r.e2e_ms:>6.0f}ms{extra} P={r.prompt_tokens:>4} C={r.completion_tokens:>4}{errtail}", file=sys.stderr)
    wall = time.perf_counter() - t_start
    return results, wall


def pct(values, p):
    if not values:
        return 0.0
    s = sorted(values)
    k = max(0, min(len(s) - 1, int(round((p / 100.0) * (len(s) - 1)))))
    return s[k]


def summarize(results, wall):
    ok = [r for r in results if r.ok]
    err = [r for r in results if not r.ok]
    total_completion = sum(r.completion_tokens for r in ok)
    total_prompt = sum(r.prompt_tokens for r in ok)
    e2e = [r.e2e_ms for r in ok]
    ttft = [r.ttft_ms for r in ok if r.ttft_ms is not None]
    per_req_tps = [
        r.completion_tokens / (r.e2e_ms / 1000.0)
        for r in ok if r.e2e_ms > 0 and r.completion_tokens > 0
    ]
    # Error breakdown by HTTP status
    status_counts = {}
    for r in err:
        key = str(r.status) if r.status else "transport"
        status_counts[key] = status_counts.get(key, 0) + 1
    return {
        "ok": len(ok), "err": len(err),
        "wall_s": wall,
        "throughput_tps": total_completion / wall if wall > 0 else 0,
        "total_completion": total_completion,
        "total_prompt": total_prompt,
        "e2e_avg_ms": statistics.mean(e2e) if e2e else 0,
        "e2e_p50_ms": pct(e2e, 50),
        "e2e_p95_ms": pct(e2e, 95),
        "e2e_p99_ms": pct(e2e, 99),
        "per_req_tps_p50": pct(per_req_tps, 50),
        "per_req_tps_p95": pct(per_req_tps, 95),
        "ttft_p50_ms": pct(ttft, 50),
        "ttft_p95_ms": pct(ttft, 95),
        "status_counts": status_counts,
    }


def render_report(args, all_rows):
    out = []
    now = time.strftime("%Y-%m-%d %H:%M:%S")
    out.append(f"# LLM Proxy 并发吞吐基准报告")
    out.append("")
    out.append(f"- **Model**: `{args.model}`")
    out.append(f"- **Endpoint**: `{args.base_url}/chat/completions`")
    out.append(f"- **每并发档样本**: {args.requests_per_level} 次")
    out.append(f"- **并发档**: {args.concurrency_sweep}")
    out.append(f"- **采集时间**: {now}")
    out.append("")
    # Executive summary
    out.append("## TL;DR")
    out.append("")
    stream_rows = [(c, s) for m, c, s in all_rows if m == "stream"]
    nonstream_rows = [(c, s) for m, c, s in all_rows if m == "nonstream"]
    if nonstream_rows:
        best_ns = max(nonstream_rows, key=lambda x: x[1]["throughput_tps"])
        out.append(f"- **非流式最大总吞吐**: **{best_ns[1]['throughput_tps']:.1f} completion tokens/s** @ 并发 {best_ns[0]}（E2E P50 {best_ns[1]['e2e_p50_ms']:.0f}ms · P95 {best_ns[1]['e2e_p95_ms']:.0f}ms）")
    if stream_rows:
        best_s = max(stream_rows, key=lambda x: x[1]["throughput_tps"])
        ttft_low = min(stream_rows, key=lambda x: x[1]["ttft_p50_ms"])
        out.append(f"- **流式最大总吞吐**: **{best_s[1]['throughput_tps']:.1f} completion tokens/s** @ 并发 {best_s[0]}")
        out.append(f"- **流式最低 TTFT (打字机起手)**: **{ttft_low[1]['ttft_p50_ms']:.0f}ms** @ 并发 {ttft_low[0]}（P95 {ttft_low[1]['ttft_p95_ms']:.0f}ms）")
    out.append("")
    # Tables
    for mode in ("nonstream", "stream"):
        rows_for_mode = [(c, s) for m, c, s in all_rows if m == mode]
        if not rows_for_mode:
            continue
        out.append(f"## {'非流式 (POST /chat/completions)' if mode == 'nonstream' else '流式 SSE (stream: true)'}")
        out.append("")
        if mode == "stream":
            out.append("| 并发 | 总耗时 | 总吞吐 | 单请求 P50 | OK / Err | E2E P50 | E2E P95 | TTFT P50 | TTFT P95 |")
            out.append("|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
            for c, s in rows_for_mode:
                out.append(
                    f"| {c} | {s['wall_s']:.1f}s | **{s['throughput_tps']:.1f}** tok/s | "
                    f"{s['per_req_tps_p50']:.1f} tok/s | {s['ok']} / {s['err']} | "
                    f"{s['e2e_p50_ms']:.0f}ms | {s['e2e_p95_ms']:.0f}ms | "
                    f"{s['ttft_p50_ms']:.0f}ms | {s['ttft_p95_ms']:.0f}ms |"
                )
        else:
            out.append("| 并发 | 总耗时 | 总吞吐 | 单请求 P50 | OK / Err | E2E P50 | E2E P95 | E2E P99 |")
            out.append("|---:|---:|---:|---:|---:|---:|---:|---:|")
            for c, s in rows_for_mode:
                out.append(
                    f"| {c} | {s['wall_s']:.1f}s | **{s['throughput_tps']:.1f}** tok/s | "
                    f"{s['per_req_tps_p50']:.1f} tok/s | {s['ok']} / {s['err']} | "
                    f"{s['e2e_p50_ms']:.0f}ms | {s['e2e_p95_ms']:.0f}ms | {s['e2e_p99_ms']:.0f}ms |"
                )
        out.append("")
        # Error breakdown
        errs = {}
        for c, s in rows_for_mode:
            for k, v in s["status_counts"].items():
                errs[k] = errs.get(k, 0) + v
        if errs:
            out.append("**错误分布**: " + ", ".join(f"`{k}` × {v}" for k, v in sorted(errs.items())))
            out.append("")
    # Glossary
    out.append("## 指标说明")
    out.append("")
    out.append("- **总吞吐 (completion tokens/s)** — 这批窗口里所有请求的 `completion_tokens` 之和 ÷ wall-clock 秒。代表服务在该并发下的**总产能**。")
    out.append("- **单请求 P50** — 每次请求自己的 `completion_tokens / e2e_seconds` 中位数。代表**单流体感**。")
    out.append("- **E2E P50 / P95 / P99** — 客户端发出到完整响应到达的端到端延迟分位。")
    out.append("- **TTFT P50 / P95 (仅流式)** — Time-To-First-Token，从请求发出到首个 `content` delta 到达的时间。决定打字机起手是否流畅。")
    out.append("")
    out.append("## 阅读提示")
    out.append("")
    out.append("- 总吞吐随并发**先增后平**，平台位即服务上限；继续加压只会拉高 E2E 延迟。")
    out.append("- 若 P95/P99 远大于 P50，说明存在排队/重试或上游瞬时拥塞。")
    out.append("- 大量 `429` 说明被本站 RPM 限制砸到 — 该 token 的 `rate_limit_rpm` 不够大。")
    return "\n".join(out)


async def main():
    p = argparse.ArgumentParser()
    p.add_argument("--base-url", default=DEFAULT_BASE_URL)
    p.add_argument("--token", default=DEFAULT_TOKEN)
    p.add_argument("--model", default=DEFAULT_MODEL)
    p.add_argument("--concurrency-sweep", default="1,2,4,8")
    p.add_argument("--requests-per-level", type=int, default=10)
    p.add_argument("--mode", choices=["stream", "nonstream", "both"], default="both")
    p.add_argument("--report", default="/tmp/proxy-bench-report.md")
    p.add_argument("--raw", default="/tmp/proxy-bench-raw.json")
    args = p.parse_args()

    missing = [name for name, val in (
        ("--token / POLAR_PROXY_TOKEN", args.token),
        ("--base-url / POLAR_PROXY_BASE_URL", args.base_url),
        ("--model", args.model),
    ) if not val]
    if missing:
        print(f"ERROR: missing required: {', '.join(missing)}", file=sys.stderr)
        print("       See doc/proxy-benchmarking.md", file=sys.stderr)
        sys.exit(2)

    levels = [int(x.strip()) for x in args.concurrency_sweep.split(",") if x.strip()]
    modes = ["nonstream", "stream"] if args.mode == "both" else [args.mode]

    all_rows = []
    raw_out = {"meta": vars(args), "batches": []}
    for mode in modes:
        for c in levels:
            print(f"\n=== mode={mode} concurrency={c} requests={args.requests_per_level} ===", file=sys.stderr)
            results, wall = await run_batch(c, args.requests_per_level, mode, args.base_url, args.token, args.model)
            s = summarize(results, wall)
            all_rows.append((mode, c, s))
            raw_out["batches"].append({
                "mode": mode, "concurrency": c, "wall_s": wall,
                "summary": s,
                "results": [r.__dict__ for r in results],
            })
            print(
                f"  -> ok={s['ok']}/{s['ok']+s['err']} "
                f"throughput={s['throughput_tps']:.1f} tok/s | "
                f"e2e p50={s['e2e_p50_ms']:.0f}ms p95={s['e2e_p95_ms']:.0f}ms"
                + (f" | ttft p50={s['ttft_p50_ms']:.0f}ms" if mode == "stream" else ""),
                file=sys.stderr,
            )

    report = render_report(args, all_rows)
    with open(args.report, "w") as f:
        f.write(report)
    with open(args.raw, "w") as f:
        json.dump(raw_out, f, indent=2, default=str)
    print(f"\nReport: {args.report}", file=sys.stderr)
    print(f"Raw:    {args.raw}", file=sys.stderr)
    print()
    print(report)


if __name__ == "__main__":
    asyncio.run(main())
