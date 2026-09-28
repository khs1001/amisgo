# amisgo 项目长期备忘

## 项目定位

`github.com/khs1001/amisgo`：Go 通用 amis 页面渲染引擎，纯渲染库、不做 HTTP 集成。

## 核心设计（已与用户逐项确认，勿随意更改）

- 公开 API 仅 4 个：`New(root string, fss ...fs.FS)`、`Render(cxt context.Context, sign string, data map[string]any) (map[string]any, error)`、`AddHook(Hook)`、`RegisterFS(fs.FS)`
- 特征码 = `text/template` 自定义定界符 `Delims = ["\"{{", "}}\""]`（定界符含两侧双引号，保证 JSON 文件合法）；特征码必须整个字符串值以 `{{` 开头；内部字符串参数用**反引号 raw string**（JSON 中无需转义）
- **引擎不做任何语法树改写（用户明确要求移除 wrapActions）**，页面按规范编写：`{{.x | json}}` 值注入（内置 json 函数编码，缺失/nil→null）；`{{include `sign` . | json}}` 加载模板（include 结果也要 `| json`；单参= nil data）
- 查找顺序：本地根目录（os.DirFS）→ RegisterFS 注册的 fs 列表按注册顺序；同名层级回退：完整路径逐级去前导目录段（user/list → list），每个候选先查完所有来源再回退层级（层级优先于来源）；sign 防穿越校验（fs.ValidPath + 拒反斜杠）
- include 内置函数（参考 GoFrame gview include 嵌入方式）：布局模式 = include 组合约定（布局页 `{{include .mainTpl . | json}}`），无 layout 函数/字段；MaxIncludeDepth=64 防循环（goroutine 级状态继承 cxt 与深度）
- 缓存：进程内、无公开失效方法（重启刷新）；cacheEntry = 模板 + 基准渲染页（nil data）；渲染输出与基准一致时零拷贝返回共享 map（只读约定）；注册过 Hook 后每次深拷贝再执行 Hook
- 最终 JSON 解析用 `UseNumber()`，数字为 `json.Number`
- sign 无 `.json` 后缀；无默认页，未找到即报错

## 用户协作偏好

- 偏好 grill 式逐项确认：一次一个问题、给选项（A/B/C），逐步定稿后再实现
