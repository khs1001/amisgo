# amisgo

Go 通用 [amis](https://aisuda.bce.baidu.com/amis/) 页面渲染引擎：将本地根目录或 `fs.FS` 列表中的 `<sign>.json` 页面文件渲染为 `map[string]any`。

纯渲染库，不绑定任何 HTTP 框架——`data` 由调用方构造，返回的页面 JSON 可直接下发给 amis 前端消费。

## 特性

- **特征码模板**：页面 JSON 中以 `"{{` 开始、`}}"` 结束（定界符包含两侧双引号）的模板动作，值注入时用内置 `json` 函数编码（`{{.x | json}}`），动作输出原位替换（含两侧引号），**渲染结果仍是合法 JSON**
- **规范透明**：引擎不做语法树改写，特征码即标准 `text/template` 语法，按规范编写即可
- **类型保真注入**：经 `| json` 编码后，数字、字符串、布尔、对象、数组按真实类型注入，而非字符串拼接
- **缺失安全**：变量缺失或为 `nil` 时输出 `null`；`data` 为 `nil` 也照常渲染
- **多来源查找**：本地根目录最优先，随后按注册顺序查找 `fs.FS` 列表（`embed.FS`、`os.DirFS` 等均可）
- **同名层级回退**：完整路径未命中时逐级回退短名称（`user/list` → `list`），短名称即通用模板
- **include 与布局模式**：内置 `include` 函数加载其他模板（对象注入、支持嵌套与循环引用防护），布局模式通过 include 组合实现（参考 GoFrame gview）
- **路径安全**：拒绝 `..`、绝对路径、反斜杠等非法 `sign`，防路径穿越
- **进程内缓存**：模板与基准渲染页缓存复用，重启刷新
- **零拷贝共享页**：渲染输出与基准一致时直接返回缓存中的共享页面（只读约定）
- **Hook 钩子**：注册 Hook 后每次渲染在独立深拷贝的页面上执行，可按 `cxt` 动态改写
- **数字精度**：解析使用 `json.Number`，大整数（如雪花 ID）不丢失精度
- **context 贯穿**：`cxt` 支持超时与取消控制

## 安装

```bash
go get github.com/khs1001/amisgo
```

## 快速开始

页面文件 `pages/user/list.json`：

```json
{
  "type": "page",
  "title": "用户列表",
  "data": {
    "userId": "{{.session.userId | json}}",
    "user": "{{.session.user | json}}",
    "tags": "{{.tags | json}}"
  }
}
```

渲染代码：

```go
package main

import (
	"context"
	"fmt"

	"github.com/khs1001/amisgo"
)

func main() {
	eng := amisgo.New("./pages") // 本地根目录最优先

	page, err := eng.Render(context.Background(), "user/list", map[string]any{
		"session": map[string]any{
			"userId": 42,
			"user":   map[string]any{"name": "tom", "age": 18},
		},
		"tags": []any{"admin", "vip"},
	})
	if err != nil {
		panic(err)
	}

	fmt.Print(page)
	// map[data:map[user:map[age:18 name:tom] userId:42 tags:[admin vip]] title:用户列表 type:page]
}
```

特征码替换后的 `data` 字段实际内容（保持合法 JSON、类型保真）：

```json
{
  "userId": 42,
  "user": {"name": "tom", "age": 18},
  "tags": ["admin", "vip"]
}
```

## 与 embed.FS 一起使用

```go
//go:embed pages
var embedded embed.FS

eng := amisgo.New("./pages") // 本地 ./pages 最优先
eng.RegisterFS(embedded)     // 其次查找 embed.FS
eng.RegisterFS(os.DirFS("./more")) // 再次查找其他 fs
```

查找顺序：**本地根目录 → fs 列表按注册顺序**，第一个命中即生效；全部未命中返回错误。

## API

### `func New(root string, fss ...fs.FS) *Engine`

创建引擎。`root` 为本地页面根目录（传空字符串表示不使用本地目录），`fss` 为备用文件系统列表。

### `func (e *Engine) RegisterFS(fsys fs.FS)`

追加备用文件系统，查找顺序位于本地根目录之后、按注册顺序依次匹配。

### `func (e *Engine) AddHook(h Hook)`

注册渲染钩子：

```go
type Hook func(cxt context.Context, sign string, page map[string]any, data map[string]any) error
```

Hook 在页面解析完成之后、返回之前被调用，可修改 `page`。注册第一个 Hook 之后，`Render` 每次返回独立深拷贝的页面（可自由修改）；未注册 Hook 时行为见下文返回值约定。

### `func (e *Engine) Render(cxt context.Context, sign string, data map[string]any) (map[string]any, error)`

渲染页面。流程：查找并编译（缓存命中则跳过）→ 以 `data` 执行特征码模板 → 解析为 `map[string]any`（数字为 `json.Number`）→ 执行 Hook（如有）。

返回值约定：

| 场景 | 返回值 |
|------|--------|
| 渲染输出与缓存基准一致（页面不含特征码，或 `data` 不影响输出） | 缓存中的**共享页面**，调用方**不得修改**，否则会污染缓存并引发并发问题 |
| 注册过 Hook | 每次独立深拷贝的页面，可自由修改 |
| 其余情况（如 `data` 影响输出） | 独立解码的页面，可自由修改 |

## 特征码语法

特征码即 `text/template` 自定义定界符（`Delims = ["\"{{", "}}\""]`）的模板动作。由于左定界符吞掉前引号、右定界符吞掉后引号，`"{{...}}"` 整体（含两侧引号）被动作输出替换——因此**特征码必须写在 JSON 字符串值的位置（整个字符串值以 `{{` 开头）**。

引擎不做任何语法树改写，**页面作者按规范编写**：值注入时用内置 `json` 函数编码（经 `json.Marshal`，缺失/`nil` 输出 `null`），保证替换后仍是合法 JSON：

| 页面写法 | data 值 | 渲染结果 |
|----------|---------|----------|
| `"{{.count \| json}}"` | `42` | `42` |
| `"{{.name \| json}}"` | `"tom"` | `"tom"` |
| `"{{.user \| json}}"` | `{"age":18}` | `{"age":18}` |
| `"{{.enabled \| json}}"` | `true` | `true` |
| `"{{.missing \| json}}"` | （缺失） | `null` |
| `"{{.nilv \| json}}"` | `nil` | `null` |

支持 `text/template` 的字段链式访问（`"{{.session.user.name | json}}"`）；特征码内部如需字符串参数（如函数实参），使用**反引号 raw string**（如 `{{include `sign` .}}`）——反引号在 JSON 中无需转义，源文件因此始终是合法 JSON。

## 同名层级回退与短名称加载

查找按**回退链**进行：从完整路径逐级去掉前导目录段，每个候选路径（`.json` 后缀）都先在全部来源（本地根目录 → fs 注册顺序）中查找，未命中再回退下一层级：

```
sign = "user/list"
① user/list.json ：本地根目录 → fs1 → fs2 → ...   （完整路径）
② list.json      ：本地根目录 → fs1 → fs2 → ...   （短名称回退）
```

- **隐式短名称**：`Render("user/list")` 完整路径未命中时自动回退到 `list.json`（通用模板），模块可用自己的 `user/list.json` 覆盖
- **显式短名称**：`Render("list")` 直接加载根层级的 `list.json`
- 缓存以原始 `sign` 为键，命中哪一级文件在首次编译时固化
- 层级优先于来源：fs 中的 `user/list.json` 优先于本地根目录的 `list.json`

## include：加载其他模板与布局模式

内置 `include` 函数（参考 [GoFrame gview 模板布局](https://goframe.org/docs/core/gview-layout) 的 include 嵌入方式）：

```json
{"side": "{{include `layout/sidebar` . | json}}"}        // 以当前上下文（dot）为 data
{"side": "{{include `layout/sidebar` .sub | json}}"}     // 以显式 data 渲染
{"side": "{{include `layout/sidebar` | json}}"}          // 以 nil data 渲染（纯静态区块）
```

- include 的结果同样是值注入，规范上也需要 `| json` 编码
- 目标页走常规查找（含同名层级回退）与按 sign 缓存，渲染结果作为**对象值**注入当前位置
- `include` 可嵌套组合，调用链深度上限 `MaxIncludeDepth`（64），超过即视为循环引用报错；仅可在 `Render` 过程中使用

**布局模式**即 include 组合约定（gview 同款用法）——布局页动态引入主内容：

```json
// pages/layout/main.json（布局页）
{
  "type": "page",
  "header": "{{include `layout/header` . | json}}",
  "body":   "{{include .mainTpl . | json}}",
  "footer": "{{include `layout/footer` . | json}}"
}
```

```go
// 调用：布局页 + 动态子页 + 共享业务数据
page, err := eng.Render(ctx, "layout/main", map[string]any{
    "mainTpl": "user/list",
    "userId":  42,
})
```

子页与布局页特征码共享同一份 `data`，`cxt` 亦被继承。

## 缓存说明

- 按 `sign` 缓存编译后的模板与基准渲染页，进程运行期内**不自动失效**，页面文件修改后需重启进程刷新
- `Engine` 并发安全，模板可被多个 goroutine 同时执行

## 测试

```bash
go test ./... -v
```

如需竞态检测（需 cgo 与 gcc）：

```bash
CGO_ENABLED=1 go test ./... -race
```

## License

MIT
