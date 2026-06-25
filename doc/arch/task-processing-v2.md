# 任务处理机制 v2 — 统一机队任务流水线

Status: **DESIGN (2026-06-25)**. 把当前散落的三条任务路径收敛成**一套**机队任务机制,
并补齐影片处理(filmscan)真正"被 agent 托管"所缺的三件东西:**中间进度上报、架构路由
(x86 抽帧 / ANE 转写)、多阶段衔接(extract→analyze)**。以 polar-film 为首个落地场景,
机制本身通用于任何需要把活派到 Mac 机队跑的模块(lawyer / photo / 未来)。

---

## 0. 为什么要做(现状的乱)

测绘发现**三条重叠的任务路径**,没有统一:

| 路径 | 表 / 端点 | 状态机 | 回调 | 消费者 | 用途 |
|---|---|---|---|---|---|
| **A. compute-tasks**(生产级) | `agent_tasks` · `/internal/v1/compute-tasks` 入队、`/api/agent/compute-tasks/claim`+`/complete` | hold→queued→running→done/failed/cancelled(+pause/resume/priority) | ✅ 签名 HMAC 回调 `requester_module+callback_path` | **Swift agent**(PolarAgentKit)+ Go agent | lawyer OCR/diarize、TTS、faces |
| **B. research-tasks**(遗留) | `project_research_runs` · `/api/agent/tasks/claim` | queued→running→succeeded/failed | ✗ | 仅 **Go agent** | LLM research run |
| **C. film process**(半死) | `/internal/v1/agent-tasks/enqueue` kind=`filmscan_extract` | job_id | ✗ 无回调 | **无正经消费者** | film `handleMovieProcess` |

**结论:** A 是唯一成熟的(claim/complete/签名回调齐全,lawyer 已验证)。C 是 film 自己造的孤儿
(没人认领、没回调)→ 所以 filmscan **只能手动 SSH 跑**,由此引出本会话踩的所有坑:
推送到错 host(404)、token 过期、scan_status 得手动 set。**这些坑全是"绕过托管链路"的症状。**

**v2 决策:film(及所有新模块任务)统一走 A(compute-tasks),废弃 C;B 保持现状(LLM research 专用)。**
在 A 上补三个能力,filmscan 即成为机队托管任务。

---

## 1. compute-tasks 现状(v2 的基座)

`agent_tasks` 表关键列:`id(tk_) · workspace_id · skill · input_json · status · agent_id ·
result_json · error · requester_module · requester_ref · callback_path · callback_status · 时间戳`。

生命周期:
```
模块 POST /internal/v1/compute-tasks (HMAC)  →  hold(默认)
   └ POST .../:id/release                    →  queued
agent POST /api/agent/compute-tasks/claim {skills:[…]}  (FOR UPDATE SKIP LOCKED) → running (盖 agent_id)
agent POST /api/agent/compute-tasks/complete/:id {ok,output,error,artifacts[]}   → done | failed
   └ dock fireTaskCallback(goroutine, 3×retry) → 签名 POST plugin.endpoint+callback_path
        body: {task_id, requester_ref, skill, status, result, error, artifacts[{kind,asset_id,mime,download_url}]}
        模块 sdk.VerifyDockCallback(HMAC + ±300s)
```
技能:`speech.stt · speech.diarize · vision.ocr · vision.faces · speech.tts`(PolarAgentKit Skills/,
均"lifted from filmscan stages")。**缺:中间进度、arch 过滤、多阶段。**

---

## 2. v2 要补的三件事

### 2a. 中间进度上报(驱动"处理中")

**问题:** 任务只有终态(done/failed)。Swift agent 内部有 `SkillEvent.progress(0..1,msg)` 但不落库、不上报。

**加:**
- `agent_tasks` += `progress_pct REAL · progress_stage TEXT · progress_at TIMESTAMPTZ`(全 nullable)。
- 新端点 **`PATCH /api/agent/compute-tasks/:id/progress {pct, stage}`**(agent-token,幂等,仅 `running`)。
- PolarAgentKit `AgentRunner.runClaimedTask` 在每个 skill 阶段把 `SkillEvent.progress` 经 `DockClient`
  PATCH 上去(节流,≥2s 或 stage 变化才发)。
- **可选签名进度回调**:模块若设了 `callback_path`,dock 可在 stage 变化时也回调一条 `status:"running"`
  (轻量、best-effort),让模块实时更新自己的状态投影(见 §4)。否则模块轮询 `GET .../:id`。

→ 通用产物:**compute-tasks.html 队列页**和**模块自己的页**都能显示"第几阶段 / 百分比"。

### 2b. 架构路由(x86 抽帧 / ANE 转写)

**问题:** claim 只按 `skill` 过滤,不看 arch;`constraints_json` 预留未用。但 filmscan 是**两段式**:
extract(解码+抽帧+脸,**x86 也行**)、analyze(WhisperKit/FluidAudio,**要 ANE/arm64**)。

**加:**
- 任务 `constraints_json` 支持 `{"required_arch":"arm64"|"x86_64"|null, "needs_ane":true|false}`。
- `claimTask` 的 SQL 增加过滤:agent claim 时带上自己的 `arch`/`has_ane`(agent 已在 host_info 里有
  [[project-host-info-tiers]] 的 `iface_kind`/arch;复用),`WHERE constraints 兼容`。
- agent `skill.advertise` 已带 capabilities;扩展为带 `{arch, ane}` 能力位。
→ extract 任务任意机器抢;analyze 任务只有带 ANE 的 arm64 Mac(如 .80)能抢 → 自动落到对的机器,
不再像手动那样人肉挑 .35/.80。

### 2c. 多阶段衔接(extract → analyze → done)

**问题:** filmscan = extract(产出 音频→music库 track + 关键帧→photo库 + manifest)→ analyze(从 manifest
拉音频做 ANE 转写 → 字幕回 film)。两阶段要顺序衔接 + 跨机器(x86→arm64)。

**选回调链式(lawyer 已用的模式),不引入新"pipeline"概念:**
```
film handleMovieProcess
  → submit compute-task  skill="film.extract"  input={video_url, media_id}  constraints={}      callback=/internal/v1/film/scan-callback  ref=media_id
  → release → 机队 x86/arm64 任一认领 → 跑 extract → complete(output={audio_track_id, keyframe_count, manifest_asset_id})
  → dock 回调 film /scan-callback (stage=extract done)
       film: setScanStatus(media, "extracted")  +  submit 下一阶段:
       → compute-task  skill="film.analyze"  input={manifest_asset_id, media_id}  constraints={required_arch:"arm64",needs_ane:true}  callback=/internal/v1/film/scan-callback  ref=media_id
       → release → 仅 ANE Mac 认领 → 跑 analyze → complete(output={srt_asset_id, segments, speakers})
  → dock 回调 film /scan-callback (stage=analyze done)
       film: 取 srt artifact → 落 subtitles(走现有 handleSubtitleUpload 内部逻辑)→ setScanStatus(media,"done")
```
单一 callback 端点 `/internal/v1/film/scan-callback` 按 `skill` 分支处理 extract/analyze 两个 stage。
**任何一阶段 failed → setScanStatus(media,"failed", error)。**

### 2d. filmscan 怎么变成机队技能

两个选项,**推荐 B(短期)→ A(长期)**:

- **选项 A(原生技能,长期对):** 把 filmscan 的 stage 拆进 PolarAgentKit 现有技能 ——
  extract = 新 `film.extract` skill(解码/抽帧/脸,代码本就是 filmscan stages 的来源);
  analyze = 复用 `speech.stt`+`speech.diarize`(已有)。纯 Swift、无外部二进制、进度天然可上报。
  代价:Swift 重写 + 重建 PolarAgentKit。
- **选项 B(跑 filmscan 二进制,短期快):** agent 新增一个 `film.extract`/`film.analyze` skill,
  其实现就是 `Process` 调用机器上的 `filmscan` 通用二进制(已在 .35/.80),把 stdout 的
  `keyframes: N/M` 解析成 progress 上报。代价小、复用现成 filmscan;缺点是依赖外部二进制 + 解析日志。

---

## 3. scan_status = 任务状态的投影(退掉手动)

本会话已建的 `media_items.scan_status/scan_detail/scan_updated_at` + `POST /movies/:id/scan-status` + UI chip/轮询
**数据模型对**,只是之前被我**手动 curl 驱动**。v2 里它由任务生命周期驱动:

| 任务事件 | scan_status | scan_detail |
|---|---|---|
| handleMovieProcess 入队 extract | `extracting` | "排队中 / 抽帧中" |
| extract 进度 PATCH | `extracting` | "抽帧 1200/2491" |
| extract done → 入队 analyze | `extracted` | "转写排队" |
| analyze 进度 | `analyzing` | "转写中 45%" |
| analyze done(字幕落库) | `done` | "字幕已就绪" |
| 任一 failed | `failed` | error 摘要 |

`POST /movies/:id/scan-status` 端点保留(给 agent/手动兜底 + 调试),但**正路是 callback 驱动**。
UI chip + 8s 轮询不变。字幕到达自动置 done 的钩子保留(双保险)。

---

## 4. 通用任务可见性

- **compute-tasks.html**(已部署):统一看队列(skill/status/agent/进度/回调结果),所有模块的机队任务都在这。
- **模块自己的页**(film 电影页 chip、lawyer 文档状态):看本模块关心的那条任务的投影状态。
- 两者数据同源(`agent_tasks` 行),前者全局、后者按 `requester_ref` 过滤。

---

## 5. 分阶段落地

- **P0(本会话已到):** scan_status schema+端点+UI(手动驱动);filmscan 手动两段式跑通一部片。
- **P1 — 接管(小):** film `handleMovieProcess` 改走 compute-tasks(submit `film.extract`)+ 补
  `/internal/v1/film/scan-callback` + 回调驱动 scan_status。先用**选项 B**(agent Process 调 filmscan 二进制)。
  退掉 `/agent-tasks/enqueue` 那条孤儿路径(C)。
- **P2 — 架构路由:** claim 加 arch/ANE 过滤;extract→analyze 跨机器自动落位。
- **P3 — 进度:** `progress_*` 列 + PATCH 端点 + agent 上报 + UI 实时百分比。
- **P4 — 原生技能(选项 A):** filmscan stages 并入 PolarAgentKit,去掉外部二进制依赖;说话人归属做
  faces↔voices 视觉融合(当前 audio-only 给 0 说话人)。

---

## 6. 落地清单(P1 最小可用)

- **dock:** 无新增(compute-tasks A 已全;复用 submit/claim/complete/callback)。
- **polar-film:** `handleMovieProcess` 改为 `SubmitComputeTask(skill=film.extract,…,callback=/internal/v1/film/scan-callback)`;
  新增 `scan_callback.go`(验签 → 按 skill 分支:extract done 入队 analyze、analyze done 落字幕 + setScanStatus)。
  退掉 `/internal/v1/agent-tasks/enqueue` 调用。
- **agent(选项 B):** PolarAgentKit 加 `FilmExtractSkill`/`FilmAnalyzeSkill`(Process 调 `filmscan extract/analyze`,
  解析进度);advertise 加这两个 skill;analyze skill 标记 needs ANE。
- **SDK:** `SubmitComputeTask` 已有;补 `required_arch` 透传即可。

相关:[[project-agent-proxy-and-tasks]](compute-tasks 系统设计)、[[project-host-info-tiers]](arch/ANE 能力来源)、
[[project-film-tmdb]] / project-filmscan、`doc/arch/agent-proxy-and-task-dispatch.md`。
