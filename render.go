package amisgo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"runtime"
	"sync"
	"text/template"
)

// jsonFuncName 是内置 JSON 编码函数名：动作求值结果经它编码为合法 JSON 字面量
// （数字/字符串/对象/数组/布尔按真实类型注入；缺失/nil 输出 null）。
// 页面按规范显式使用：{{.x | json}}。
const jsonFuncName = "json"

// includeFuncName 是内置模板函数名，用于加载其他模板（参考 GoFrame gview 的
// include 嵌入方式），布局模式即通过它组合实现。
const includeFuncName = "include"

// MaxIncludeDepth 是 include 调用链的最大深度（含布局嵌套），
// 超过即视为循环引用，返回错误。
const MaxIncludeDepth = 64

// encodeJSON 将动作求值结果编码为合法 JSON 字面量。
func encodeJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// compile 解析页面文件并编译模板。引擎不做任何语法树改写，
// 页面作者按规范编写特征码：
//
//	{{.x | json}}                 值注入（JSON 编码，保证合法性）
//	{{include `sign` .}}          以当前上下文为 data 加载其他模板
//	{{include `sign` .sub}}       以显式 data 加载其他模板
//	{{T `标题` | json}}           调用方经 Funcs 注册的自定义函数
//
// 用户函数先注册、内置函数后注册：与内置函数同名时以内置函数为准。
func compile(e *Engine, name string, src []byte) (*template.Template, error) {
	e.mu.RLock()
	userFuncs := e.funcs
	e.mu.RUnlock()

	t := template.New(name).Delims(`"{{`, `}}"`)
	if len(userFuncs) > 0 {
		t = t.Funcs(userFuncs)
	}
	t = t.Funcs(template.FuncMap{
		jsonFuncName:    encodeJSON,
		includeFuncName: e.includeFunc,
	})
	t, err := t.Parse(string(src))
	if err != nil {
		return nil, fmt.Errorf("amisgo: 解析页面 %q 失败: %w", name, err)
	}
	return t, nil
}

// ---- 渲染状态：goroutine 级，用于 include 继承外层 Render 的 cxt 与深度 ----

type renderState struct {
	cxt   context.Context
	depth int
}

var renderStates sync.Map // goroutine id (uint64) -> *renderState

var goidPattern = regexp.MustCompile(`^goroutine (\d+)`)

// goid 返回当前 goroutine 的 id。
func goid() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	m := goidPattern.FindSubmatch(buf[:n])
	if m == nil {
		return 0
	}
	var id uint64
	for _, c := range m[1] {
		id = id*10 + uint64(c-'0')
	}
	return id
}

func pushRenderState(cxt context.Context) { renderStates.Store(goid(), &renderState{cxt: cxt}) }

func popRenderState() { renderStates.Delete(goid()) }

func currentRenderState() *renderState {
	if v, ok := renderStates.Load(goid()); ok {
		return v.(*renderState)
	}
	return nil
}

// includeFunc 是内置 include 函数（参考 GoFrame gview 的 include 嵌入方式）：
//
//	{{include `sign` .}}      以当前上下文（dot）为 data 渲染目标页
//	{{include `sign` .sub}}   以显式 data 渲染目标页
//	{{include `sign`}}        以 nil data 渲染目标页（纯静态区块）
//
// 目标页走常规查找（本地根目录 -> fs 注册顺序 + 同名层级回退）与按 sign 缓存，
// 渲染结果（map[string]any）作为对象值注入当前位置。
// include 可嵌套（布局组合即基于此），调用链深度超过 MaxIncludeDepth 时报错。
func (e *Engine) includeFunc(args ...any) (any, error) {
	if len(args) < 1 || len(args) > 2 {
		return nil, fmt.Errorf("amisgo: include 需要 1~2 个参数：include `sign` [数据]")
	}
	sign, ok := args[0].(string)
	if !ok {
		return nil, fmt.Errorf("amisgo: include 的第一个参数必须是字符串 sign")
	}
	var data map[string]any
	if len(args) == 2 && args[1] != nil {
		data, ok = args[1].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("amisgo: include 的 data 参数必须是对象，实际为 %T", args[1])
		}
	}

	st := currentRenderState()
	if st == nil {
		return nil, fmt.Errorf("amisgo: include 只能在 Render 过程中使用")
	}
	if st.depth >= MaxIncludeDepth {
		return nil, fmt.Errorf("amisgo: include 嵌套深度超过 %d（可能存在循环引用）", MaxIncludeDepth)
	}
	st.depth++
	defer func() { st.depth-- }()

	return e.render(st.cxt, sign, data)
}

// cacheEntry 是 sign 的缓存项。
type cacheEntry struct {
	tmpl *template.Template

	// 基准渲染（以 nil data 执行一次的结果）：
	// 当某次渲染的输出与基准完全一致（例如页面不含特征码，
	// 或 data 产生的输出恰好相同）时，可直接零拷贝返回 basePage。
	baseBytes []byte
	basePage  map[string]any
}

// loadEntry 返回 sign 对应的缓存项，带进程内缓存。
// 模板对象并发执行安全；缓存内容在进程运行期内不自动失效（重启刷新）。
func (e *Engine) loadEntry(sign string) (*cacheEntry, error) {
	if v, ok := e.cache.Load(sign); ok {
		return v.(*cacheEntry), nil
	}
	src, err := e.lookup(sign)
	if err != nil {
		return nil, err
	}
	tmpl, err := compile(e, sign, src)
	if err != nil {
		return nil, err
	}
	entry := &cacheEntry{tmpl: tmpl}
	// 计算基准渲染；失败不影响使用（仅失去零拷贝共享能力）。
	// 注：含 include 的页面在基准渲染（nil data）时通常无法组合出有效布局，
	// 此类页面自动退化为每次独立解码。
	if out, err := execTemplate(tmpl, nil); err == nil {
		if page, err := decodePage(out); err == nil {
			entry.baseBytes, entry.basePage = out, page
		}
	}
	// 并发下可能重复编译，以先存入者为准。
	actual, _ := e.cache.LoadOrStore(sign, entry)
	return actual.(*cacheEntry), nil
}

// execTemplate 以 data 执行模板，返回渲染文本。
func execTemplate(t *template.Template, data map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Render 将 sign 对应的页面文件渲染为 map[string]any。
//
// 流程：查找并编译（缓存命中则跳过）→ 以 data 执行特征码模板
// → 解析为 map[string]any（数字为 json.Number）→ 执行 Hook（如有）。
//
// 查找规则：回退链中每个候选路径（完整路径最优先，逐级去掉前导目录段得到短名称）
// 都先在全部来源（本地根目录 -> fs 注册顺序）中查找。
//
// 返回值约定：
//   - 渲染输出与缓存基准一致时（页面不含特征码，或 data 不影响输出），
//     返回缓存中的共享页面，调用方不得修改，否则会污染缓存并引发并发问题；
//   - 注册过 Hook 后，每次渲染返回独立深拷贝的页面，可自由修改；
//   - 其余情况返回独立解码的页面。
//
// cxt 用于取消与超时控制，并被 include 的子渲染继承；data 可为 nil（缺失变量输出 null）。
func (e *Engine) Render(cxt context.Context, sign string, data map[string]any) (map[string]any, error) {
	if cxt == nil {
		cxt = context.Background()
	}
	if err := cxt.Err(); err != nil {
		return nil, err
	}

	pushRenderState(cxt)
	defer popRenderState()
	return e.render(cxt, sign, data)
}

// render 是渲染的实际执行体，供 Render 与 includeFunc 复用。
func (e *Engine) render(cxt context.Context, sign string, data map[string]any) (map[string]any, error) {
	entry, err := e.loadEntry(sign)
	if err != nil {
		return nil, err
	}

	out, err := execTemplate(entry.tmpl, data)
	if err != nil {
		return nil, fmt.Errorf("amisgo: 渲染 %q 失败: %w", sign, err)
	}
	if err := cxt.Err(); err != nil {
		return nil, err
	}

	var page map[string]any
	if entry.basePage != nil && bytes.Equal(out, entry.baseBytes) {
		// 与基准渲染一致：零拷贝返回共享页面。
		page = entry.basePage
	} else {
		if page, err = decodePage(out); err != nil {
			return nil, fmt.Errorf("amisgo: sign %q: %w", sign, err)
		}
	}

	if e.hasHook.Load() {
		page = deepCopyPage(page)
		for _, h := range e.hooksSnapshot() {
			if err := h(cxt, sign, page, data); err != nil {
				return nil, fmt.Errorf("amisgo: hook for %q: %w", sign, err)
			}
		}
	}
	return page, nil
}

// hooksSnapshot 返回当前 Hook 列表的副本。
func (e *Engine) hooksSnapshot() []Hook {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]Hook, len(e.hooks))
	copy(out, e.hooks)
	return out
}

// decodePage 将渲染后的文本解析为 map[string]any。
// 数字解析为 json.Number 以避免大整数精度丢失；
// 页面必须是单个 JSON 对象，且不允许有尾随内容。
func decodePage(b []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()

	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("渲染结果不是合法 JSON: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("渲染结果包含尾随内容")
	}

	page, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("页面顶层必须是 JSON 对象，实际为 %T", v)
	}
	return page, nil
}

// deepCopyPage 深拷贝页面（map/slice 递归复制，叶子值不可变直接共享）。
func deepCopyPage(v map[string]any) map[string]any {
	out := make(map[string]any, len(v))
	for k, val := range v {
		out[k] = deepCopyValue(val)
	}
	return out
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, vv := range t {
			m[k] = deepCopyValue(vv)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, vv := range t {
			s[i] = deepCopyValue(vv)
		}
		return s
	default:
		return v
	}
}
