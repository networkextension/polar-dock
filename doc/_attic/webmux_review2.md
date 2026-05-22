# 根因定位：终端上方留白 / 下方输入区不可见

## 一句话结论

全局 CSS 中的 `textarea { width:100%; min-height:320px; ... }` 规则**误伤了 xterm.js 内部隐藏的 `.xterm-helper-textarea`**，把它撑成了 1228×320px 的实体盒子，把真正的终端行容器 `.xterm-rows` 整体向下推了 ~320px，所以：

- 终端面板顶部出现一大块"留白"（其实是被撑高的 helper-textarea 占位）
- 终端最后几行（包括当前命令行 `local@... %` 和光标）被推到 `.xterm-screen` 边界之外，被 `overflow:hidden` 裁掉了

## 关键观测数据

| 元素 | 期望 | 实际 |
|---|---|---|
| `.xterm-screen` | 容纳 helpers + rows | `1228 × 1064`，y: 93–1157 ✅ |
| `.xterm-helpers` | 高度 ~0（仅放隐藏 textarea） | **高度 337.5px** ❌ |
| `.xterm-helper-textarea` | `position:absolute; width:~7px; height:~14px` | `position:static; width:1228px; height:320px` ❌ |
| `.xterm-rows` (含全部输出+光标) | 起点 y≈93 | 起点 **y=430.5**，终点 **y=1494.5**（超出 screen.bottom=1157，被裁掉 337px） ❌ |

helper-textarea 的 inline style 实际只有：
```
left: 216.706px; top: 1106px; line-height: 14px; z-index: -5;
```
没有 `position`、`width`、`height` 三个关键属性，于是被全局规则接管。

## 引发问题的 CSS 规则（在页面内联样式表中）

```css
textarea {
  width: 100%;
  min-height: 320px;
  background: var(--card);
  border: 1px solid var(--border);
  border-radius: 10px;
  color: var(--text);
  padding: 12px 14px;
  font-family: inherit;
  resize: vertical;
}
```

这条规则本意是让"表单里的多行输入框"长得统一好看，但选择器太宽，把 xterm 内部用作 IME 代理的隐藏 textarea 也一并改写了。

## 现场验证

我在控制台手动执行：

```js
const ta = document.querySelector('.xterm-helper-textarea');
ta.style.setProperty('position','absolute','important');
ta.style.setProperty('height','14px','important');
ta.style.setProperty('width','7px','important');
```

修复后立即：
- `.xterm-helpers` 高度从 337.5 → 0
- `.xterm-rows` 从 y=430.5 → y=93，整段历史日志和当前命令行 `local@locals-Mac-2 research %` 都出现在面板内（截图已确认）

## 推荐修复方案（按优先级）

1. 收紧全局 textarea 规则（最简单，强烈推荐）
   把 `textarea { ... }` 改为只对应用内自定义的 textarea 生效，例如：
   ```css
   .form textarea, .input-textarea, textarea.input {
     width: 100%;
     min-height: 320px;
     /* ...其余样式... */
   }
   ```
   或者用 `:not()` 排除 xterm：
   ```css
   textarea:not(.xterm-helper-textarea) { ... }
   ```

2. 提高 xterm 容器的样式特异性（防御式修复）
   在 console 的样式文件里加：
   ```css
   .console-pane-term .xterm-helper-textarea {
     position: absolute !important;
     min-height: 0 !important;
     height: auto !important;
     width: auto !important;
     padding: 0 !important;
     border: 0 !important;
     resize: none !important;
     background: transparent !important;
   }
   .console-pane-term .xterm-helpers { position: absolute; top: 0; left: 0; }
   ```
   这样即使将来再有人写宽泛的 `textarea` 规则也不会再砸到终端。

3. 顺带处理 `.xterm-width-cache-measure-container`
   这个测宽用容器目前 `position: static` + 14px 高度，会在 `.xterm-helpers` 内额外占 14px。建议在上面同一选择器里加 `position: absolute; visibility: hidden; top: 0; left: 0;` 完全脱离文档流。

4. CSS reset / 全局表单样式重排
   长期看，建议把"表单控件外观"的样式集中到一个 `.form-control` / `.input` 类上，避免直接给 `input` / `textarea` / `select` 这类原生标签做全局重写——`select`、`input` 在 xterm、富文本、第三方组件里都会被注入，类似的"误伤"还会再发生。

## 备注：其它会被同一规则误伤的位置

由于该规则匹配所有 `textarea`，凡是第三方组件（图表、富文本、终端、表情选择器、CodeMirror/Monaco 的辅助 textarea 等）插入的隐藏 textarea，都会被强制 `width:100%; min-height:320px;`。建议在改这条规则的同时全站搜索类似的"裸标签"全局样式（如 `input { ... }`、`select { ... }`、`button { ... }`），统一收紧选择器。
