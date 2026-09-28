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
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

// Hook 在每次渲染、页面解析完成之后被调用，可对 page 做最终修改。
//
// 仅当引擎注册过 Hook 时，Render 才会在深拷贝出的页面上执行 Hook；
// 未注册任何 Hook 时 Render 直接返回缓存中的共享页面，
// 此时 Hook 与共享页面的只读约定均不适用。
type Hook func(cxt context.Context, sign string, page map[string]any, data map[string]any) error

// Engine 将 sign.json 页面文件渲染为 map[string]any。
//
// Engine 并发安全：模板与页面缓存可被多个 goroutine 同时使用。
type Engine struct {
	mu      sync.RWMutex
	root    fs.FS   // 本地根目录（os.DirFS 包装），nil 表示无本地根目录
	fss     []fs.FS // 备用文件系统列表，按注册顺序查找
	hooks   []Hook
	hasHook atomic.Bool

	// cache: sign (string) -> *template.Template（已完成特征码包装）
	cache sync.Map
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

// lookup 查找页面文件：回退链中每个候选路径（完整路径最优先）
// 都先在全部来源（本地根目录 -> fs 注册顺序）中查找，未命中再回退下一层级。
func (e *Engine) lookup(sign string) ([]byte, error) {
	if _, err := pageFile(sign); err != nil {
		return nil, err
	}
	candidates := fallbackPaths(sign)

	e.mu.RLock()
	root, fss := e.root, e.fss
	e.mu.RUnlock()

	sources := make([]fs.FS, 0, len(fss)+1)
	if root != nil {
		sources = append(sources, root)
	}
	sources = append(sources, fss...)

	for _, file := range candidates {
		for _, src := range sources {
			if src == nil {
				continue
			}
			b, err := fs.ReadFile(src, file)
			if err == nil {
				return b, nil
			}
		}
	}
	return nil, fmt.Errorf("amisgo: 页面 %q 未找到（已尝试 %v）", sign, candidates)
}
