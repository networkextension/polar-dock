# Console 页面 UI 缺陷检查报告

页面地址：`https://zen.4950.store:2443/console.html?x`

下面按"严重程度 → 类别"梳理本次自动化检查发现的 UI 实现缺陷。

## 一、严重缺陷（影响功能 / 易出错）

1. xterm 终端 viewport 高度为 0，内容贴底显示
   - `.xterm-viewport` 实测尺寸：`width=1243, height=0`，但 `.xterm-scrollable-element` 的 `scrollHeight=1416, clientHeight=1064`。
   - 表现：第一个终端面板上方出现一大片黑色空白，shell 输出（`bash-3.2$`、内核日志等）全部紧贴底部显示，用户感觉"内容被推到下面"。建议检查 xterm 的 `FitAddon` 初始化时机，或为 `.console-pane-term` 设置正确的 `flex: 1; min-height: 0`。

2. "关闭全部"按钮可能触发原生 `confirm()` 导致渲染线程长时间冻结
   - 测试中点击 `#consoleCloseAllBtn` 触发了一次 45s+ 的 JS 执行超时，截图恢复后布局自动重置为 1 pane。
   - 建议改用应用内 modal 二次确认，避免阻塞主线程，同时禁止在无可关闭面板时仍触发提示。

3. "保存布局"使用原生 `window.prompt` 收集名称
   - 默认值是 `布局 5/15/2026, 10:11:11 PM`（带秒、AM/PM），既不符合产品 UI 风格，也无校验（空名、重名、超长都未处理）。
   - 建议替换为统一风格的对话框，并加入名称必填 / 去重 / 最大长度等校验。

4. 保存成功后下拉框更新存在明显延迟（异步刷新）
   - 调用 `POST /api/console/layouts` 后立刻读取 `#consoleSavedSelect`：选项未更新、`#consoleDeleteBtn` 仍处于 `disabled`，约 1s 后才出现新条目。
   - 建议保存接口成功回调中立即把新布局插入下拉并选中，避免用户误以为"保存失败再点一次"造成重复条目。

## 二、布局 / 视觉缺陷

5. 顶部工具栏元素垂直对齐不一致
   - `#consoleLayoutSelect`、`#consoleSavedSelect` 高度 19px，按钮 `#consoleSaveBtn / #consoleDeleteBtn / #consoleCloseAllBtn` 高度 43–46.5px，全部位于同一行但 y 起点分别是 15.75 / 6 / 7.75。视觉上下拉框明显比按钮"矮一截"，建议统一控件高度（推荐 32–36px）。

6. "🗑"删除按钮在 disabled 状态下仍保留 `cursor: pointer`
   - `#consoleDeleteBtn` 实测 `disabled=true` 但 `cursor=pointer`，鼠标悬停时仍提示可点击，违反禁用态视觉规范。建议禁用态加 `cursor: not-allowed; opacity: 0.5`。

7. 2×2 / 3×3 布局下空面板（"未选择"）启动表单错位、出现大面积空白
   - 启动表单（host / skill / 启动按钮）被垂直居中后下方剩余大片黑色空白；当窗口较高时尤为明显。建议将表单包裹层改为顶部对齐或加 padding 让视觉重心更合理。

8. "未选择"面板的关闭按钮（红色 ×）尺寸/触达区偏小
   - `.console-pane-head .btn-inline` 实测尺寸 22.8 × 15.5px，低于 WCAG 建议的最小可点击区域 24×24。

9. 面板标题文本溢出策略基本正确，但缺少 `title` 属性
   - `.console-pane-title` 已设置 `text-overflow: ellipsis; white-space: nowrap`，但截断后没有 `title` / `aria-label`，悬停看不到完整内容。

## 三、可访问性 / 语义缺陷

10. 所有面板内的 `<select>` 都缺乏标签关联
    - host、skill、shell 三个下拉框 `id` 为空、无 `aria-label`、无 `<label for>`、无 `title`；屏幕阅读器只能读到 placeholder 文本。

11. 顶部按钮无 `aria-label`
    - `#consoleDeleteBtn` 文本是单个 emoji 🗑，但 `aria-label` 为 null（仅有 `title="删除选中的已保存布局"`，对部分辅助技术不够）。`#consoleCloseAllBtn` 没有 `title` 也没有 `aria-label`。

12. 工作区入站消息红点显示 "0" 且仍被渲染
    - `.lp-ws-inbox-dot` 文本是 "0"，DOM 上 `display: none`，但语义上应当在无消息时移除节点或保持空文本，避免被屏幕阅读器读出 "0"。

13. 隐藏的 shell 选择器仍存在于 DOM 中
    - `.console-pane-empty` 的第三个 `<select>`（"默认 shell (auto) / bash / zsh / sh"）使用 `hidden` 属性隐藏，但仍可被 Tab 键聚焦到（部分浏览器行为）。建议改用 `display:none` 或加 `tabindex="-1"`。

## 四、行为 / 一致性问题

14. "主机"链接（`.lp-topbar-back`）样式像按钮但其实是 `<a>`
    - `href=/hosts.html`、无 `target`。点击会整页跳转，丢失当前 Console 状态。建议改为 SPA 路由或加 "返回主机列表（会清空当前布局）" 之类的二次确认。

15. 终端连接信息有视觉残留
    - 终端区域可看到一行 `$$$$$$$$$$...` 似乎是 PROMPT 或欢迎横幅的残影，与下面 `[console] connected · run=19` 重叠/不齐。建议清理欢迎信息的渲染逻辑。

16. 控制台异常（非本页面发出）
    - 控制台中有 3 条 `Error: Accessibility tree function not found. Please refresh the page.`，来自 `console.html:0`。需要确认是否为业务代码或扩展注入；如属业务方应捕获处理。

## 检查覆盖范围
- 1/2/3/2×2/2×3/3×3 全部布局切换
- 工具栏的"布局/已保存/保存/删除/关闭全部/主机"全部控件
- 面板头部、关闭按钮、空状态表单
- xterm 终端内部尺寸、滚动条
- localStorage 与 `/api/console/layouts` 的 POST/GET 流程
- 控制台异常、可访问性属性、cursor / 焦点行为

> 注：测试中创建的 `MyLayoutX`、`TestLayout1` 两条临时已保存布局已在结束前调用删除接口清理完毕，未在用户数据中残留。

如果需要，我可以针对其中任意一条缺陷给出具体的 CSS/JS 修复建议或最小复现步骤。
