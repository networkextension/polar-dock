# Post 块式编辑器(方案 C)设计 + 跨平台一键复制

2026-07-12 · 状态:设计定向(用户选定 C),待 spike 验证后开工
前篇比选:`doc/ui/post-composer-design.md`

## 1. 产品形态

Notion/小红书式块编辑:

- **块类型 v1**:段落 / 标题 / 列表 / 引用 / 代码块(StarterKit 已有)+ 新增 **figure**(单图:caption、对齐、宽度)、**gallery**(2-9 图组)、**video**、**file**(附件卡片)、分割线。
- **交互**:`/` 斜杠菜单插块、块左侧拖拽手柄排序、图片粘贴/拖拽即时上传(块内显示进度)、gallery 内拖动排序。
- 技术底座不换:现有 Tiptap v3.22(`lib/rich-editor.ts`)向上扩展,不引入新编辑器框架。

## 2. 关键架构决策:存储格式

**分叉点:Tiptap JSON vs markdown + 受限 directive。选后者。**

```markdown
:::figure{align=center width=80}
![水坝全景](/uploads/xxx.jpg)
黄昏时的水坝,长曝 30s
:::

:::gallery
![](/uploads/a.jpg)
![](/uploads/b.jpg)
![](/uploads/c.jpg)
:::
```

理由:

1. **优雅降级是关键属性**:fence 内是标准 markdown,不认识 directive 的渲染器(旧移动端、AI 摘要、搜索索引、GitHub 预览)仍能看到图和文字,只是丢排版。JSON 存储则是全有或全无。
2. 语法向 `remark-directive` / `markdown-it-container` 生态靠拢,web 端有现成 parser 可改。
3. `html:false` 安全模型不破:存储里永远没有原始 HTML,directive 属性走白名单(align/width/cols 等枚举值)。
4. 将来真要 JSON(协同编辑等),markdown→JSON 无损升级路径存在;反向没有。

代价(诚实计入):directive parser 要在**每个渲染端**各实现一次(web marked 插件 + Swift 端);属性语法要文档化冻结。

## 3. 难度清单(按模块,诚实评估)

| 模块 | 内容 | 难度 | 当量 |
|---|---|---|---|
| 编辑器 node views | figure/gallery/video/file 四个自定义 node + NodeView(选中态、caption 内联编辑、上传进度占位) | **高**(体力+细节) | 4-5 天 |
| 斜杠菜单 + 拖拽手柄 | Tiptap v3 无官方 slash menu,要么手写 suggestion 插件要么借鉴社区实现;drag handle 用 @tiptap/extension-drag-handle | 中 | 2 天 |
| 序列化 | tiptap-markdown 0.9 每个 node 自定义 `markdown.serialize/parse`;gallery/figure ↔ directive 双向 | 中(有扩展点,但 parse 侧要接 markdown-it 插件) | 2 天 |
| web 渲染 | `lib/marked.ts` 加 container 解析 + figure/gallery/video HTML 渲染 + sanitize 属性白名单;feed 卡片和详情页两处 | 中 | 2 天 |
| **移动端(最大长尾)** | polar-sdk-swift 的 markdown 渲染不认 directive → 手写 fence 预处理(正则切块)或先降级(fence 当普通段落,图仍显示) | **高**(一致性回归无止境) | 先降级 0 天,完整 3-5 天 |
| 上传管线 | `POST /api/posts/media`(复用缩略图管线、workspace 归属)+ 孤儿媒体 pending/清理 | 低-中 | 1-2 天 |
| feed 摘要兼容 | 服务端从 directive/`![]()` 提取图片登记 post_images(九宫格、封面、旧端) | 低 | 1 天 |
| 回归 | 旧帖(纯 markdown + 帖尾画廊)在新渲染器下不变形;编辑旧帖的往返保真 | 中 | 1-2 天 |

**总量 ~2.5-3 周当量**(移动端走降级则 ~2 周)。最难的三处:① node view 交互打磨;② 多端渲染一致性(尤其 Swift);③ 下面的公众号导出管线。

## 4. 一键复制到外部平台

机制:`navigator.clipboard.write(ClipboardItem)` 同时写两个 flavor——`text/html`(排版保真)+ `text/plain`(markdown 兜底)。目标平台逐个评估:

### Notion — 兼容性最好,近乎免费 ✅
Notion 粘贴 HTML 的块转换质量高(h1-h6/list/quote/code/img 全对应);粘贴 markdown 纯文本也能自动转块。我们的 figure/gallery 会降级为连续 image 块 + caption 段落——可接受。**风险低,直接覆盖。**

### 微信公众号 — 可行但规则苛刻,是导出功能的主要工作量 ⚠️
公众号编辑器粘贴 HTML 的已知规则(mdnice / doocs-md 这类工具存在的原因):

1. **只认内联样式**:class 和样式表全剥,必须做「主题 CSS → style 属性」内联管线(juice 类库,或导出时遍历 DOM 计算样式写回)。代码块、列表缩进、引用条都要按公众号的怪癖特殊处理。
2. **图片是最大坑**:粘贴时编辑器会抓外链图转存微信 CDN。我们的图挂在 `zen.4950.store:2443`(非标端口 + 未备案域名),**抓取很可能失败**。对策(按优先级):
   - 导出时把图片全部转 **base64 data URI** 内嵌进 HTML —— 传闻公众号粘贴 base64 图会自动转存,**需 spike 实测**;
   - 兜底:导出「HTML + 图片打包下载」,人工逐张上传。
3. 主题:v1 做一套朴素主题即可(标题色、引用条、代码块底色),不做 mdnice 那种主题市场。

### 小红书 — 本质不是富文本平台,需求要重定义 🔁
小红书笔记 = **图片组(≤18 张)+ 纯文本文案**,web 端不接受富文本粘贴。「一键发到小红书」的正确形态是:

- **图文卡片导出**:把块内容渲染成 3:4 竖版长图/分页卡片(前端 canvas 或服务端 headless Chrome 截图),用户下载图组;
- 文案(纯文本 + #话题)进剪贴板。

这是一个独立子功能(卡片模板、分页断点、字号),建议单列 P3,不要和复制功能混在一起承诺。

### 通用兜底 — 复制为 markdown ✅
`text/plain` 写 markdown 原文:Obsidian/Typora/语雀/GitHub 全吃。零成本,顺手就有。

## 5. 分期

- **C-P0(核心,~1.5-2 周)**:figure/gallery/video/file node + 斜杠菜单 + 即时上传(`/api/posts/media`)+ directive 序列化 + web 渲染插件;移动端走降级显示。
- **C-P1**:post_images 提取、孤儿媒体清理、旧帖回归、Swift 端完整 directive 渲染。
- **C-P2(一键复制)**:通用富文本复制(内联样式 HTML + base64 图 + markdown plain)→ 覆盖 Notion + 公众号;公众号主题一套。
- **C-P3**:小红书图片卡片导出。
- **前置**:先做 POLARDOCKU #7(静态资源缓存失效),编辑器这种大 JS 变更不能再裸发。

## 6. 开工前 Spike(各半天,验证假设)

1. **公众号**:粘贴 base64 `<img>` 是否自动转存;`:2443` 外链图能否被抓取。→ 决定 C-P2 图片策略。
2. **tiptap-markdown 0.9** 自定义 node 的 `serialize/parse` 写 directive 的实际手感;parse 侧接 markdown-it-container 的路径。→ 决定序列化层工作量估算是否成立。
3. **Notion 粘贴保真度**:我们生成的 HTML 各块类型的落地效果。
4. **Swift 降级表现**:现移动端渲染器遇到 `:::figure` fence 的实际显示(不能是乱码/吞图)。

Spike 结果回填本文档后再排 C-P0。

## 7. Spike 结果(2026-07-12)

**Spike 2 — tiptap-markdown directive 往返:✅ 4/4 通过,扩展点够用。**
PoC 在 dock-ui 分支 `spike/tiptap-directive`(不合并),headless 测试 figure(带/不带属性)、gallery、纯 markdown 均往返保真。机制确认:序列化 = 每 node 的 storage `markdown.serialize`;解析路径 = markdown-it → HTML → node 的 `parseHTML`;markdown-it 实例可从 `editor.storage.markdown.parser.md` 拿到(0.9 未文档化但可用)。两个实坑已记录:① markdown-it-container **默认 validator 用整个 info 串和名字精确比较**,`:::figure{align=…}` 会被拒——必须自定义 `validate` 接受 `名字{属性}` 形态;② fence 内的 `![图]\ncaption` 被渲染成 `<p>`,caption 要从 figure 的 `textContent` 取而不是找 `<figcaption>`。C-P0 序列化层工作量估算(2 天)成立。

**Spike 4 — Swift 降级:✅ 风险清零,且属"新建"不是"改造"。**
polar-sdk-swift 的 `PolarUI` 目前是占位符——**移动端今天没有任何 post markdown 渲染器**。对未来最可能的底座 Apple `AttributedString(markdown:, .full)` 实测:`:::` 行降级为普通段落文本(不吞不崩),**fence 内图片照常出 imageURL run**。P0 移动端降级完全可接受;C-P1 是全新实现(fence 正则预切块 → 原生视图),无历史包袱。

**Spike 1/3 — 公众号/Notion 粘贴:载荷已就绪,待人工两步。**
测试页 `https://zen.4950.store:2443/spike-clipboard.html`(注意:下次 UI 部署 rsync --delete 会带走它,需重放)。一键写入 text/html(内联样式标题/引用/代码块 + base64 图 + :2443 外链图 + caption 形态)+ text/plain(markdown)。待观察项:公众号两张图哪张存活、样式保留度;Notion 各块转换质量。结果出来回填此节。

**#7 缓存失效(前置项)进展**:dock-ui PR #52(构建期 `?v=buildHash` 盖章,40/41 页)+ zen/opera/.77 三台 nginx `.html` → `Cache-Control: no-cache` 已生效(响应头已验证)。PR merge 后部署即闭环。

## 8. C-P0a 实施记录(2026-07-12)

代码完成,CI 绿,待 merge + 配对部署:

- **后端 polar-dock#473**:`POST /api/posts/media`(field=file,image/* 限 20MB)→ 复用 post 图片管线(480/1280 缩略)→ 三档变体注册为 **draft 公开资产**(`posts/drafts/<mediaID>/…`,metadata `{draft, media_id}`,即 C-P1 孤儿 GC 的目标集)→ 返回 `{media_id, url, medium_url, small_url}`(永久 `/api/media/<id>`,资产失败回退 /uploads/)。无 schema 改动。`registerPostMediaAssetNamed` 从原函数抽出,原调用方行为不变。
- **前端 dock-ui#53**:`lib/blocks/directive-nodes.ts`(Figure/Gallery node + 编辑器侧 markdown-it 接线)、`lib/blocks/marked-directive.ts`(渲染侧 marked v15 block extension)、rich-editor 工厂加 extensions/editorProps/setupMarkdown 三个口、工具栏 插图/插图组 按钮 + 粘贴/拖拽上传、内联图点击放大。**双 emitter 输出同构 HTML + 内联样式**(zen /styles.css alias 到 polar-ui-shared,仓库 CSS 会被静默忽略——内联同时利好 C-P2 复制保真)。
- **回归工装**:`blocks-test.html` + `scripts/blocks-test.mjs`(8 用例:往返×3、渲染×5 含 `:::script` 降级与 `javascript:` 过滤),8/8 过,后续块类型扩展在此加用例。
- P0a 范围裁剪:video/file 块、斜杠菜单、拖拽手柄、块内占位、交互式 caption 编辑 → P0b。
