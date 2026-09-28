// Package amisgo 是一个通用的 amis 页面渲染引擎。
//
// 它将本地根目录或 fs.FS 列表中的 <sign>.json 页面文件渲染为 map[string]any：
// 页面文件是合法的 JSON 文本，其中可包含引擎特征码 —— 以 "{{ 开始、以 }}"
// 结束的模板动作（定界符包含两侧双引号）。特征码即标准 text/template 语法，
// 引擎不做语法树改写，页面按规范编写：
//
//	{{.x | json}}                值注入（内置 json 函数编码，缺失/nil 输出 null）
//	{{include `sign` .}}         加载其他模板（布局模式即由此组合实现）
//
// 动作输出原位替换（含两侧引号），保证渲染结果仍是合法 JSON。
//
// 查找按同名层级回退链进行（完整路径最优先，逐级去掉前导目录段得到短名称），
// 每个候选路径都先在全部来源（本地根目录 -> fs 注册顺序）中查找。
//
// 用法示例：
//
//	//go:embed pages
//	var embedded embed.FS
//
//	eng := amisgo.New("./pages")       // 本地根目录最优先
//	eng.RegisterFS(embedded)           // 其次查找 embed.FS
//
//	page, err := eng.Render(context.Background(), "user/list", map[string]any{
//	    "session": map[string]any{"userId": 42},
//	})
package amisgo

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
)

// ErrNotFound 是「无此页面」的哨兵错误：磁盘根目录与全部已注册 fs 中都
// 未命中目标页面时，查找错误会包装它返回，调用方可用
// errors.Is(err, ErrNotFound) 程序化区分「无此页面」与「页面存在但坏了」。
var ErrNotFound = errors.New("amisgo: page not found")

// Hook 在每次渲染、页面解析完成之后被调用，可对 page 做最终修改。
//
// 仅当引擎注册过 Hook 时，Render 才会在深拷贝出的页面上执行 Hook；
// 未注册任何 Hook 时 Render 直接返回缓存中的共享页面，
// 此时 Hook 与共享页面的只读约定均不适用。
type Hook func(cxt context.Context, sign string, page map[string]any, data map[string]any) error

// Engine 将 sign.json 页面文件渲染为 map[string]any。
//
// Engine 并发安全：模板与页面缓存可被多个 goroutine 同时使用。
// Funcs/Strict 等链式设置方法应在首次 Render 前调用完成。
type Engine struct {
	mu     sync.RWMutex
	root   fs.FS   // 本地根目录（os.DirFS 包装），nil 表示无本地根目录
	fss    []fs.FS // 备用文件系统列表，按注册顺序查找
	funcs  template.FuncMap
	strict bool
	hooks  []Hook
	hasHook atomic.Bool

	// cache: sign (string) -> *template.Template（已完成特征码包装）
	cache sync.Map
}

// source 是带标签的查找来源，读取失败时用于错误定位。
type source struct {
	name string
	fsys fs.FS
}

// Funcs 注册调用方模板函数（如 i18n 翻译器），可在页面特征码中调用：
//
//	eng.Funcs(template.FuncMap{"T": translator})
//	页面: {"title": "{{T `标题` | json}}"}
//
// 链式设置，返回引擎自身。与内置函数（json、include）同名时以内置函数为准。
func (e *Engine) Funcs(funcs template.FuncMap) *Engine {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.funcs == nil {
		e.funcs = template.FuncMap{}
	}
	for name, fn := range funcs {
		e.funcs[name] = fn
	}
	return e
}

// Strict 开启严格寻址模式：查找只按完整路径精确命中，禁用同名层级回退链
// （"user/list" 不再回退 "list"）。适合「sign 即权限路径」的场景，避免
// 权限路径被低层级同名模板意外命中。链式设置，返回引擎自身。
func (e *Engine) Strict() *Engine {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.strict = true
	return e
}

// New 创建引擎。
//
// root 为本地页面根目录路径，查找优先级最高；root 为空字符串表示不使用本地目录。
// fss 为备用文件系统列表（如 embed.FS、fstest.MapFS 等），
// 在本地根目录未命中时按注册顺序依次查找。
func New(root string, fss ...fs.FS) *Engine {
	e := &Engine{fss: append([]fs.FS(nil), fss...)}
	if root != "" {
		e.root = os.DirFS(root)
	}
	return e
}

// RegisterFS 追加一个备用文件系统，查找顺序位于本地根目录之后、
// 先前注册的文件系统之后（按注册顺序依次匹配）。
func (e *Engine) RegisterFS(fsys fs.FS) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.fss = append(e.fss, fsys)
}

// AddHook 注册渲染钩子。注册第一个 Hook 之后，
// Render 的返回值将改为每次渲染独立深拷贝的页面。
func (e *Engine) AddHook(h Hook) {
	if h == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.hooks = append(e.hooks, h)
	e.hasHook.Store(true)
}

// pageFile 校验 sign 并返回对应的页面文件相对路径。
//
// 规则：sign 非空；不含反斜杠；满足 fs.ValidPath
// （拒绝 ".."、绝对路径、以 "/" 开头结尾、空路径元素等）。
// 文件路径为 sign + ".json"，sign 中不应携带 .json 后缀。
func pageFile(sign string) (string, error) {
	switch {
	case sign == "":
		return "", fmt.Errorf("amisgo: sign 为空")
	case sign == ".":
		return "", fmt.Errorf("amisgo: 非法 sign %q", sign)
	case strings.ContainsRune(sign, '\\'):
		return "", fmt.Errorf("amisgo: 非法 sign %q：不允许反斜杠", sign)
	case !fs.ValidPath(sign):
		return "", fmt.Errorf("amisgo: 非法 sign %q", sign)
	}
	return sign + ".json", nil
}

// fallbackPaths 生成 sign 的同名层级回退查找链：
// 从完整路径逐级去掉前导目录段，每个层级加 .json 后缀。
//
//	"user/list"      -> ["user/list.json", "list.json"]
//	"a/b/c"          -> ["a/b/c.json", "b/c.json", "c.json"]
//	"list"           -> ["list.json"]
func fallbackPaths(sign string) []string {
	parts := strings.Split(sign, "/")
	out := make([]string, 0, len(parts))
	for i := range parts {
		out = append(out, strings.Join(parts[i:], "/")+".json")
	}
	return out
}

// sourcesSnapshot 返回当前查找来源列表（本地根目录最优先，随后按注册顺序），
// 以及严格寻址开关状态。
func (e *Engine) sourcesSnapshot() (sources []source, strict bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.root != nil {
		sources = append(sources, source{name: "local", fsys: e.root})
	}
	for i, f := range e.fss {
		sources = append(sources, source{name: fmt.Sprintf("fs%d", i), fsys: f})
	}
	return sources, e.strict
}

// lookup 查找页面文件。
//
// 常规模式：回退链中每个候选路径（完整路径最优先，逐级去掉前导目录段）
// 都先在全部来源中查找，未命中再回退下一层级。
// 严格模式（Strict）：只按完整路径精确命中。
//
// 读取遵循 fail-fast：来源中文件存在但读取失败（错误并非「不存在」）时
// 直接报错并携带来源标签，SHALL NOT 静默跳过该来源——否则陈旧副本会
// 悄悄顶上，「页面改了不生效」将无法排查。只有确定「不存在」才继续下一
// 来源；全部未命中时返回包装 ErrNotFound 的错误。
func (e *Engine) lookup(sign string) ([]byte, error) {
	file, err := pageFile(sign)
	if err != nil {
		return nil, err
	}
	sources, strict := e.sourcesSnapshot()

	candidates := []string{file}
	if !strict {
		candidates = fallbackPaths(sign)
	}

	for _, file := range candidates {
		for _, src := range sources {
			b, err := fs.ReadFile(src.fsys, file)
			if err == nil {
				return b, nil
			}
			if !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("amisgo: 读取页面文件 %q（来源 %s）失败: %w", file, src.name, err)
			}
		}
	}
	return nil, fmt.Errorf("%w: 页面 %q 未找到（已尝试 %v）", ErrNotFound, sign, candidates)
}

// Validate 对全部页面做启动期校验：按来源优先级枚举每个 <sign>.json
//（同一 sign 只校验优先级最高的来源，低优先级来源中的同名页面因被遮蔽
// 不会实际参与查找），逐个以 nil data 完整渲染（编译 + 执行 + JSON 解码），
// 任一页面失败即返回包含 sign 与来源标签的错误。
//
// 用于服务启动期 fail-fast，把页面语法错误挡在上线前。注意：页面若依赖
// data 才能完成渲染（如经 include 变量动态组合布局），静态校验可能误报。
func (e *Engine) Validate() error {
	sources, _ := e.sourcesSnapshot()

	claimed := map[string]string{} // sign -> 首个声明它的来源名
	for _, src := range sources {
		err := fs.WalkDir(src.fsys, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if p == "." && errors.Is(err, fs.ErrNotExist) {
					return fs.SkipAll // 来源整体不存在：视为空来源
				}
				return fmt.Errorf("amisgo: 遍历来源 %s 失败: %w", src.name, err)
			}
			if d.IsDir() || !strings.HasSuffix(p, ".json") {
				return nil
			}
			sign := strings.TrimSuffix(p, ".json")
			if _, ok := claimed[sign]; ok {
				return nil // 已被更高优先级来源声明，本来源的同名页面被遮蔽
			}
			claimed[sign] = src.name
			if _, err := e.Render(context.Background(), sign, nil); err != nil {
				return fmt.Errorf("amisgo: 校验页面 %q（来源 %s）失败: %w", sign, src.name, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
