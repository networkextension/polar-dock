# Post 图文编辑(图文混排)设计方案

2026-07-12 · 状态:已定向 **方案 C**(用户决定)——详细设计见 `doc/ui/post-block-editor-design.md`;本文保留比选记录

## 现状(polar-dock-ui main)

- 正文编辑器已是 **Tiptap WYSIWYG**(`lib/rich-editor.ts`:StarterKit + tiptap-markdown,`html:false`,存储纯 markdown),post.html 与 creation.html 共用。
- 但**图片/视频/附件是三个独立 file input**,发布时 multipart 一次性提交(`POST /api/posts`);图片由服务端生成 small/medium 缩略图入 `post_images`,feed 端渲染为帖尾九宫格。**图进不了正文,做不到图文混排**。
- 渲染端 `lib/marked.ts` 的 `sanitizeHtml` 为黑名单制(去 script/iframe/on*/javascript:),markdown `![alt](url)` 的 `<img>` **现在就能渲染**,无需改动。
- 移动端(polar-sdk-swift)消费同一份 markdown;post 媒体存储正在向 polar-assets 迁移(app.go 有 /uploads/ → assets 回填)。

## 方案

### A. 增强画廊模式(微博/朋友圈式,最低成本)

保持「正文 + 帖尾媒体」结构不变,只升级交互:

- 拖拽/粘贴图片进缩略图条,即时本地预览;拖动排序、单张删除、上传进度;数量/体积限制提示。
- 后端零改动(仍发布时 multipart),feed 渲染不变。

工作量:纯前端 ~2-3 天。
局限:图仍在帖尾,长图文(教程/公告/带截图的报障)表达力不够——**没有解决诉求本身**。

### B. Markdown 内联图片(GitHub/掘金式)⭐ 推荐

正文即完整图文文档:

1. **编辑器**:Tiptap 加 `@tiptap/extension-image`;工具栏插图按钮 + **粘贴/拖拽图片直接进正文**。tiptap-markdown 原生把 image node 序列化为 `![alt](url)`,`html:false` 安全模型不破。
2. **即时上传**:图片落进编辑器时立刻上传,返回 URL 后替换占位。端点二选一:
   - 新开 `POST /api/posts/media`(**倾向**):复用现有 post 图片管线 → 生成 small/medium 缩略图、归 workspace、直接可登记 `post_images`;
   - 复用 `/api/attachments`:免后端开发,但无缩略图、feed 摘要要另想。
3. **feed 摘要兼容**:发布时服务端从 markdown 提取图片(或前端随表单带 media id 列表)登记进 `post_images`——九宫格摘要、移动端旧渲染、封面图全都继续工作。
4. **孤儿媒体生命周期**:上传了但没发布的图,打 pending 标记 + 定时清理(chat 附件有同样的已知欠账,FUTURE_WORK.md 里记着,可一并做)。
5. **渲染收紧(可选)**:sanitize 时限制 `img src` 为同源 `/uploads/` + assets 域,防外链图追踪。
6. 视频/zip 附件**维持现状**(帖尾字段)——视频内联的收益撑不起成本。

工作量:前端 ~3-4 天 + 后端 ~1-2 天。
风险:低。存储仍是纯 markdown,所有下游(移动端、搜索、AI 摘要)无感。

### C. 块式图文编辑器(Notion/小红书卡片式)

Tiptap 自定义 block node:imageBlock(caption/宽度/对齐)、gallery block(多图一块)、videoBlock、fileBlock;存储要么 markdown+受限扩展语法(`:::gallery`),要么迁去 Tiptap JSON。

- 排版能力最强,是 C 端产品的终态形态。
- 但**打破"纯 markdown 可移植"**:web feed、移动端、所有 markdown 消费方(AI 摘要、搜索索引)都要同步实现块渲染;`html:false` 的安全叙事也要重新论证。
- 工作量:2 周+,且长尾成本在多端一致性。

结论:远期形态,现在不做;B 的存储格式(markdown)不阻碍将来升级到 C。

## 比选

| | A 画廊增强 | B 内联图片 ⭐ | C 块式 |
|---|---|---|---|
| 解决图文混排 | ✗ | ✓ | ✓✓ |
| 前端成本 | 低 | 中 | 高 |
| 后端成本 | 零 | 小(media 端点+提取) | 中 |
| 移动端/下游兼容 | 无感 | 无感(纯 markdown) | 全端重做 |
| 渲染端改动 | 无 | 无(已放行 img) | 大 |
| 安全面 | 不变 | 不变(可顺手收紧 img src) | 需重新论证 |

## 推荐落地路径

- **P0**:方案 B 的 1+2(编辑器内联 + 即时上传端点)——图文混排立即可用;
- **P1**:B 的 3+4(post_images 提取兼容 + 孤儿清理);
- **P2**:把 A 的交互(粘贴/拖拽/排序)补到仍保留的帖尾视频/附件字段;
- 远期:需要 caption/多图排版时再评估 C。

注意事项:发布前先解决 **POLARDOCKU #7(UI 静态资源缓存不失效)**,否则编辑器这种大 JS 变更又会出现旧缓存跑新后端的窗口。
