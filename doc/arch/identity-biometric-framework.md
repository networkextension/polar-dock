# 统一人物 / 生物特征框架（人脸 + 声纹）

Status: design · 2026-06-21 · 跨 polar-film（人脸,已有 PF-10..14）、polar-lawyer（录音声纹,新）、polar-task 舰队（产生样本的 skill）

## 1. 背景与动机
影视已有一整套人脸识别（PF-14：feature-print → pgvector → 疑似同人/找相似/跨片同人 → people 行）。
录音转写很快也要"采集声纹、归纳到具体人"（律师的当事人、会议发言人…）。**声纹本质就是人脸的音频版**：
都是「生物特征向量 → 聚类 → 归到某个人」。如果每个模块各搞一套,就会出现两份 people、两套聚类、两套
确认 UI。所以定一个**模态无关的平台级框架**:一个 `person` 实体,挂任意模态的生物特征样本;face / voice
只是 modality。

**核心难点（用户强调）**:声纹/人脸识别都不准。识别只能给「候选」,**必须保留原始片段做参考,人确认后才能
归到具体人**。这条是框架第一原则,不是事后补丁。

## 2. 核心原则
1. **原始参考永存**:每个样本都引用其来源媒体片段(人脸=帧时间戳+bbox;声纹=音频 span)+ 落成 assets。
2. **只给候选,确认才归人**:嵌入只产出 `候选 person + score`。高阈值可建议,**绝不自动并人**;低置信
   必须人工放原声/原帧对比确认后才写 `person_id`。
3. **模型可换、可重嵌入**:存了原始片段 + 模型版本,模型升级后从原始片段**重新嵌入**(对应 film 的
   "no backfill → re-run";这里是 re-embed),历史向量不锁死在旧模型。
4. **模态独立空间、靠 person 连**:face 向量空间 ≠ voice 向量空间,各自最近邻;唯一的连接点是 `person` 行
   (和 PF-14 "faces vs 台词 不同 cluster space、用 people 行 join" 同构)。
5. **生物特征 = 敏感数据**:workspace 隔离,审计,删除 person 级联删样本。

## 3. 数据模型
```
person                      -- workspace 内的"人"（= 复用 film people）
  id, workspace_id, display_name, notes, created_at

biometric_sample           -- 一次观测（人脸/声纹/未来:步态/掌纹…）
  id
  workspace_id
  modality        'face' | 'voice'
  vector          pgvector            -- 各模态自己的维度（face 768d / voice ~192-256d）
  model_id        text                -- 嵌入模型 + 版本（用于重嵌入 & 同空间比对约束）
  dim             int
  source_asset_id bigint              -- 原始媒体（帧图 / 音频 clip）在 assets 的引用
  locator         jsonb               -- face: {frame_ts,bbox}; voice: {t0,t1,recording_asset_id}
  captured_from   text                -- 'film' | 'lawyer' | 'meeting' ...
  person_id       text NULL           -- 归纳前为空
  confidence      real                -- 归纳时的判定分（人工确认=1.0）
  attributed_by   text NULL           -- 'human' | 'auto@<score>'
  created_at
-- 索引: (workspace_id, modality) + 每模态一个 ivfflat/hnsw 向量索引（同 model_id 才可比）
```
- 一个 person 的「声纹」= 其 `modality=voice` 样本集合(或其质心);「人脸」同理。
- 不同 `model_id` 的向量不跨比;换模型 = 新 model_id + 重嵌入。

## 4. 样本怎么产生（舰队 skill）
- **人脸**:已有 `vision.faces`(检测 + 768d feature-print)。改造:输出里带 source 帧定位 + 帧图 asset 引用,
  落 `biometric_sample(modality=face)`。
- **声纹**:新 `speech.diarize` skill(复用 filmscan 的 **FluidAudio** 说话人分离——它本来就把音频切成
  说话人 A/B/C 的 segment)。每个 segment 额外吐:**声纹向量** + 该 segment 的**音频 clip**(存 assets)。
  → `biometric_sample(modality=voice)`。
  端侧坑(沿用记忆):filmscan diarization 在 ANE 上 SIGSEGV → **`--compute cpu`**;声纹嵌入同样走 cpu。
  说话人分离本身**只在一段录音内区分 A/B/C**(段内一致),跨录音的"A 是同一个真人"由本框架的聚类+确认解决。
- 走 polar-task 队列(P1 input-by-asset 取原始音/视频,P3 回调把样本交回模块)。

## 5. 归纳流程（attribution）
```
新样本 v(modality, model_id)
  → 同 workspace+modality+model_id 的最近邻 (pgvector)
  → top-k 候选 person + score
  → score ≥ HIGH:    标记 "疑似 person X @score"（建议,不自动写）
    LOW < score < HIGH: 进"待确认"队列
    无候选:            "新人?" 建议建 person
  → 人工 UI:放原声 clip / 看原帧 + 候选对比 → 确认/改派/新建 → 写 person_id, attributed_by='human'
可选自动:仅当 score 极高且 modality 内多样本一致才 auto@score(仍可被人推翻)
```
"找相似 / 疑似同人 / 跨录音同人" 三个查询与 PF-14 人脸侧完全对称,只是换 modality。

## 6. 落地路径（避免过度设计）
1. **先定 `biometric_sample` 契约 + person 复用**:film 人脸迁进来当 **modality #1**(它已有 people + 向量,
   改造小;迁移期可双写)。
2. **加 voice 作 modality #2**:`speech.diarize` skill + 律师"录音转写"接它(当事人声纹归纳),共用同一 person/确认 UI。
3. **第 2 个消费者(lawyer)真用上后**,再把 person + biometric 存储从 film 抽成共享服务(`polar-identity` 或并入 dock)。
   在那之前不抽,省空转。
4. 确认 UI:先复用 film 的人物页范式(放帧 → 现在也放音频 clip)。

## 7. 隐私 / 安全
生物特征属敏感个人信息:workspace 强隔离;person/sample 删除级联;访问审计;原始 clip 与向量同享 assets 的
私有可见性;后续考虑 person 级"导出/被遗忘"。律师场景还涉及证据链:声纹 clip 即证据,需保留 hash + 来源 recording。

## 8. Open questions
- **声纹嵌入模型**:端侧可跑、cpu-friendly 的 speaker-verification 模型(维度/许可)?FluidAudio 是否已含 embedding,
  还是需独立模型?
- **同空间约束**:多 model_id 共存时检索要带 model_id 过滤;质心如何随确认增量更新。
- **person 归属冲突**:同一 segment 跨模态证据矛盾(声音像 A、脸像 B)时的裁决 = 人工。
- **抽共享服务的时机**:film 人脸要不要等抽服务时一起迁,还是先 in-place 加表。

## 9. 阶段
- **B0** 契约:`person` + `biometric_sample` schema（先在 film 库内,modality=face 迁入/双写）。
- **B1** `speech.diarize` skill(FluidAudio,cpu)→ segment + 声纹向量 + 音频 clip → biometric_sample(voice)。
- **B2** 归纳 API + 待确认队列 + 确认 UI(放原声/原帧)。律师录音转写接入。
- **B3** 第 2 消费者就绪后,抽共享 `polar-identity`(person + biometric + 检索 + 确认)。
- **B4** 模型升级 re-embed 流水线;隐私(删除/导出/审计)。

---

## 10. 重新规划 (Redesign 2026-06-23) — 统一多模态 + 半监督 + Apple-Photos 式管理

B0–B3 已落地(person + biometric_sample(face/voice) + 聚类控制台 v3:服务端说话人簇/人脸簇 + 分页)。
下一跃迁:做成 Apple-Photos 式「人物」系统。

### 10.1 统一模型(一切挂到 person)
- `person` ← `biometric_sample(face)` + `biometric_sample(voice)` + (派生)字幕行。
- **字幕不单独打标签**:字幕行有时间区间 → 落在某 speaker 的 voice `[t0,t1]` → 该 speaker 确认成 person
  后,字幕行自动带上 person。"确认一次说话人 = 声纹+字幕+(时间共现)人脸 同时打上同一 identity"。
- `media_item` 聚合一个来源 + 它衍生出的所有样本。

### 10.2 三种来源 → 统一 ingest 编排(fan-out 到现有舰队 skill)
| 来源 | 流水线 | 产出 |
|---|---|---|
| 视频 | 抽音轨→diarize(声纹+说话人段)+stt(字幕);抽关键帧→vision.faces | voice+face+字幕 |
| 音频 | diarize + stt | voice+字幕 |
| 图片 | vision.faces | face |
现状:lawyer 管音频、临时脚本管视频人脸 → 统一成一个 identity-ingest 入口(film 管视频/lawyer 管音频/新增图片路径,数据形态一致)。

### 10.3 半监督自动打标签(P1,已实现)= "机器怎么学"
**1-NN 标签传播**(复用现有 pgvector `<=>`/`candidates`):已确认样本 = 范例;每个未标注样本找同模态最近
邻 → 候选 person + 相似度。分数 ≥ auto → 自动归(`attributed_by='auto'`,可整批撤销);≥ suggest → 进建议
列表(一键确认/改判);否则留着。确认越多 → 覆盖越广 = Apple Photos「又找到这个人的 N 张」。
- 端点 `POST /api/identity/auto-label {modality,auto_threshold?,suggest_threshold?,dry_run?}` →
  `{applied[],applied_count,suggestions[]}`。
- **阈值实测**(原始 VNFeaturePrint 768d,1-NN 是单链接,比 rep-anchored 聚类更宽 → 需更高的 auto 门槛):
  face auto=**0.94**/suggest=0.84(2 个范例人 → 自动归 24、建议 41,精确);voice auto=0.80/suggest=0.65
  (wespeaker 更可分;不同说话人不会互窜)。两者 query 可覆盖。

### 10.4 人工分/合(P2,Apple-Photos 式,双向)
- **合并**:两簇/人是同一个人 → `POST /people/merge {from,into}`(改挂 from 的全部样本到 into + 删 from)。
- **拆分/移出**:一个人里混进别人 → 框选错的样本 → "不是TA → 移到新/别的人 / 退回待确认"(带 override 的
  bulk 改挂;现 attribute-bulk 只翻 NULL,需加允许覆盖已归属的模式)。
- **反向**:两张脸其实一人 → 选中合并。

### 10.5 People-album UX(P2/P3)
人物为主页(命名 + 「待命名」簇);每人:脸网格 + 声纹 clip + TA 说过的字幕行;框选移出;"找更多"(=auto-label)。

### 10.6 阶段
- **P1 自动打标签**(本轮已做):1-NN 传播 + auto-label 端点 + 控制台「✨ 自动归类」(高分自动 + 中分建议)。
- **P2 合并/拆分**:`/people/merge` + override 改挂 + 人物详情框选移出。
- **P3 统一 ingest**:一个入口吃视频/音频/图片;film+lawyer 统一 push;补图片路径。
- **P4 字幕一等公民**:存 `transcript_segment`,经 speaker→person 显示"谁说了什么" + 导出。

### 10.7 已知局限
原始 VNFeaturePrint 上做聚类/传播是启发式(Apple 的 computeDistance 才是"正规"度量,我们只存了原始向量)——
够用来排序/浮现/传播主要人物,非 identity 级。后续:存正规 feature-print 距离 / 换专用人脸识别 embedding。
