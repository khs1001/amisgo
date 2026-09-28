package amisgo

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"text/template"
)

// newFS 构造一个包含指定页面的 fstest.MapFS。
func newFS(pages map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for sign, content := range pages {
		m[sign+".json"] = &fstest.MapFile{Data: []byte(content)}
	}
	return m
}

func mustRender(t *testing.T, e *Engine, sign string, data map[string]any) map[string]any {
	t.Helper()
	page, err := e.Render(context.Background(), sign, data)
	if err != nil {
		t.Fatalf("Render(%q) 出错: %v", sign, err)
	}
	return page
}

// TestFeatureCodeTypes 验证特征码按真实类型注入（数字/字符串/对象/数组/布尔），
// 缺失变量与 nil 均输出 null。
func TestFeatureCodeTypes(t *testing.T) {
	e := New("", newFS(map[string]string{
		"types": `{"num":"{{.num | json}}","str":"{{.str | json}}","obj":"{{.obj | json}}","arr":"{{.arr | json}}","bl":"{{.bl | json}}","missing":"{{.missing | json}}","nilv":"{{.nilv | json}}"}`,
	}))
	page := mustRender(t, e, "types", map[string]any{
		"num":  42,
		"str":  "abc",
		"obj":  map[string]any{"k": "v"},
		"arr":  []any{1, "x"},
		"bl":   true,
		"nilv": nil,
	})

	want := map[string]any{
		"num":     json.Number("42"),
		"str":     "abc",
		"obj":     map[string]any{"k": "v"},
		"arr":     []any{json.Number("1"), "x"},
		"bl":      true,
		"missing": nil,
		"nilv":    nil,
	}
	if !reflect.DeepEqual(page, want) {
		t.Fatalf("结果不符\n got: %#v\nwant: %#v", page, want)
	}
}

// TestNestedFieldAndEmptyData 验证多级字段访问，以及 data 为 nil 时仍执行模板。
func TestNestedFieldAndEmptyData(t *testing.T) {
	e := New("", newFS(map[string]string{
		"nested": `{"userId":"{{.session.userId | json}}","name":"{{.session.user.name | json}}"}`,
	}))
	page := mustRender(t, e, "nested", map[string]any{
		"session": map[string]any{
			"userId": json.Number("12345678901234567890123"),
			"user":   map[string]any{"name": "tom"},
		},
	})
	want := map[string]any{
		"userId": json.Number("12345678901234567890123"),
		"name":   "tom",
	}
	if !reflect.DeepEqual(page, want) {
		t.Fatalf("结果不符\n got: %#v\nwant: %#v", page, want)
	}

	// data 为 nil：所有特征码输出 null
	page2 := mustRender(t, e, "nested", nil)
	if !reflect.DeepEqual(page2, map[string]any{"userId": nil, "name": nil}) {
		t.Fatalf("data 为 nil 时结果不符: %#v", page2)
	}
}

// TestLocalRootPriority 验证本地根目录最优先。
func TestLocalRootPriority(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "list.json"), []byte(`{"from":"local"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	e := New(root, newFS(map[string]string{"user/list": `{"from":"fs"}`}))
	page := mustRender(t, e, "user/list", nil)
	if page["from"] != "local" {
		t.Fatalf("期望本地目录优先, got %#v", page)
	}
}

// TestFSRegistrationOrder 验证 fs 列表按注册顺序查找。
func TestFSRegistrationOrder(t *testing.T) {
	e := New("", newFS(map[string]string{"page": `{"from":"second"}`}))
	e.RegisterFS(newFS(map[string]string{"page": `{"from":"third"}`}))

	page := mustRender(t, e, "page", nil)
	if page["from"] != "second" {
		t.Fatalf("期望按注册顺序命中第一个 fs, got %#v", page)
	}
}

// TestInvalidSign 验证路径穿越与非法 sign 被拒绝。
func TestInvalidSign(t *testing.T) {
	e := New(".", newFS(map[string]string{"secret": `{}`}))
	for _, sign := range []string{"", ".", "../secret", "..\\secret", "a\\b", "/abs", "a/./b", "a/../b"} {
		if _, err := e.Render(context.Background(), sign, nil); err == nil {
			t.Fatalf("sign %q 应报错，实际成功", sign)
		}
	}
}

// TestNotFound 验证全部来源未命中时报错。
func TestNotFound(t *testing.T) {
	e := New(".", newFS(map[string]string{}))
	if _, err := e.Render(context.Background(), "nope", nil); err == nil {
		t.Fatal("未命中的 sign 应报错")
	}
}

// TestInvalidRenderedJSON 验证渲染结果非合法 JSON / 有尾随内容时报错。
func TestInvalidRenderedJSON(t *testing.T) {
	e := New("", newFS(map[string]string{
		"broken":  `"{{.str}}`,    // 引号被定界符吞掉后缺少收尾 → 非法 JSON
		"trailer": `{"a":1} 尾随内容`, // 渲染后含尾随内容
		"array":   `["{{.x}}"]`,   // 顶层不是对象
	}))
	for _, sign := range []string{"broken", "trailer", "array"} {
		if _, err := e.Render(context.Background(), sign, map[string]any{"str": "abc", "x": 1}); err == nil {
			t.Fatalf("sign %q 应报错，实际成功", sign)
		}
	}
}

// TestSharedInstanceWithoutHook 验证未注册 Hook 时返回缓存共享对象（零拷贝）。
func TestSharedInstanceWithoutHook(t *testing.T) {
	e := New("", newFS(map[string]string{"p": `{"a":1}`}))
	m1 := mustRender(t, e, "p", nil)
	m2 := mustRender(t, e, "p", nil)
	if reflect.ValueOf(m1).Pointer() != reflect.ValueOf(m2).Pointer() {
		t.Fatal("未注册 Hook 时两次 Render 应返回同一共享 map")
	}
}

// TestHookCopyIsolation 验证注册 Hook 后每次渲染为独立拷贝，
// 且调用方/Hook 对返回页面的修改不会污染缓存。
func TestHookCopyIsolation(t *testing.T) {
	e := New("", newFS(map[string]string{"p": `{"a":"{{.v | json}}"}`}))
	n := 0
	e.AddHook(func(cxt context.Context, sign string, page map[string]any, data map[string]any) error {
		n++
		page["n"] = n
		page["polluted"] = true
		return nil
	})

	m1 := mustRender(t, e, "p", map[string]any{"v": 1})
	if m1["n"] != 1 || m1["a"] != json.Number("1") {
		t.Fatalf("第一次渲染结果不符: %#v", m1)
	}

	m2 := mustRender(t, e, "p", map[string]any{"v": 2})
	if m2["n"] != 2 {
		t.Fatalf("第二次渲染结果不符: %#v", m2)
	}
	if reflect.ValueOf(m1).Pointer() == reflect.ValueOf(m2).Pointer() {
		t.Fatal("注册 Hook 后每次渲染应返回独立拷贝")
	}

	// 缓存未被污染：直接检查缓存的模板原始输出不含 hook 注入字段
	entry, err := e.loadEntry("p")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := execTemplate(entry.tmpl, map[string]any{"v": 3})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"a":3}` {
		t.Fatalf("缓存被污染，原始模板输出: %s", raw)
	}
}

// TestHookError 验证 Hook 返回错误时 Render 失败。
func TestHookError(t *testing.T) {
	e := New("", newFS(map[string]string{"p": `{}`}))
	e.AddHook(func(cxt context.Context, sign string, page map[string]any, data map[string]any) error {
		return errSentinel
	})
	if _, err := e.Render(context.Background(), "p", nil); err == nil {
		t.Fatal("Hook 出错时 Render 应报错")
	}
}

var errSentinel = &hookError{}

type hookError struct{}

func (*hookError) Error() string { return "hook failed" }

// TestRegisterFSAfterRender 验证渲染后追加 fs 仍生效（未命中新页面时）。
func TestRegisterFSAfterRender(t *testing.T) {
	e := New("", newFS(map[string]string{"a": `{"v":1}`}))
	mustRender(t, e, "a", nil)
	e.RegisterFS(newFS(map[string]string{"b": `{"v":2}`}))
	page := mustRender(t, e, "b", nil)
	if page["v"] != json.Number("2") {
		t.Fatalf("追加 fs 未生效: %#v", page)
	}
}

// TestCanceledContext 验证已取消的 context 使 Render 失败。
func TestCanceledContext(t *testing.T) {
	e := New("", newFS(map[string]string{"p": `{}`}))
	cxt, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Render(cxt, "p", nil); err == nil {
		t.Fatal("已取消的 context 应使 Render 报错")
	}
}

// TestConcurrentRender 并发渲染同一页面（配合 -race 检测缓存竞态）。
func TestConcurrentRender(t *testing.T) {
	e := New("", newFS(map[string]string{"p": `{"v":"{{.v | json}}"}`}))
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			page, err := e.Render(context.Background(), "p", map[string]any{"v": i})
			if err != nil {
				t.Errorf("并发渲染出错: %v", err)
				return
			}
			if page["v"] != json.Number(itoa(i)) {
				t.Errorf("并发结果不符: %#v", page["v"])
			}
		}(i)
	}
	wg.Wait()
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

// ---- 同名层级回退 / 短名称加载 ----

// TestFallbackHierarchy 验证完整路径未命中时回退到短名称（同名层级回退）。
func TestFallbackHierarchy(t *testing.T) {
	e := New("", newFS(map[string]string{
		"list": `{"from":"short"}`,
	}))
	page := mustRender(t, e, "user/list", nil)
	if page["from"] != "short" {
		t.Fatalf("期望回退命中短名称 list.json, got %#v", page)
	}
	// 多级回退：a/b/c -> b/c -> c
	e2 := New("", newFS(map[string]string{"c": `{"from":"c"}`}))
	page2 := mustRender(t, e2, "a/b/c", nil)
	if page2["from"] != "c" {
		t.Fatalf("期望多级回退命中 c.json, got %#v", page2)
	}
}

// TestFullPathPriority 验证完整路径优先于回退层级。
func TestFullPathPriority(t *testing.T) {
	e := New("", newFS(map[string]string{
		"user/list": `{"from":"full"}`,
		"list":      `{"from":"short"}`,
	}))
	page := mustRender(t, e, "user/list", nil)
	if page["from"] != "full" {
		t.Fatalf("期望完整路径优先, got %#v", page)
	}
}

// TestFallbackCrossSourcePriority 验证层级优先于来源：
// 每个候选路径先查完所有来源，再回退下一层级。
// fs 中的 user/list.json 应优先于本地根目录的 list.json。
func TestFallbackCrossSourcePriority(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "list.json"), []byte(`{"from":"local-short"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	e := New(root, newFS(map[string]string{
		"user/list": `{"from":"fs-full"}`,
	}))
	page := mustRender(t, e, "user/list", nil)
	if page["from"] != "fs-full" {
		t.Fatalf("期望层级优先于来源（fs 完整路径命中）, got %#v", page)
	}
}

// ---- include 内置函数与布局模式 ----

// TestIncludeObjectInjection 验证 include 结果以对象注入，且默认使用当前上下文。
func TestIncludeObjectInjection(t *testing.T) {
	e := New("", newFS(map[string]string{
		"sidebar": `{"type":"sidebar","items":"{{.items | json}}"}`,
		"page":    "{\"type\":\"page\",\"side\":\"{{include `sidebar` . | json}}\"}",
	}))
	page := mustRender(t, e, "page", map[string]any{"items": []any{"a", "b"}})
	side, ok := page["side"].(map[string]any)
	if !ok {
		t.Fatalf("include 结果应为对象注入, got %#v", page["side"])
	}
	items, ok := side["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("子页特征码未按当前 data 渲染: %#v", side)
	}
}

// TestIncludeExplicitData 验证 {{include "sign" 数据}} 显式传参。
func TestIncludeExplicitData(t *testing.T) {
	e := New("", newFS(map[string]string{
		"card": `{"title":"{{.title | json}}"}`,
		"page": "{\"a\":\"{{include `card` .a | json}}\",\"b\":\"{{include `card` .b | json}}\"}",
	}))
	page := mustRender(t, e, "page", map[string]any{
		"a": map[string]any{"title": "A"},
		"b": map[string]any{"title": "B"},
	})
	if page["a"].(map[string]any)["title"] != "A" || page["b"].(map[string]any)["title"] != "B" {
		t.Fatalf("include 显式 data 不生效: %#v", page)
	}
}

// TestIncludeFallback 验证 include 的目标 sign 同样享受同名层级回退。
func TestIncludeFallback(t *testing.T) {
	e := New("", newFS(map[string]string{
		"widget": `{"kind":"widget"}`,
		"page":   "{\"w\":\"{{include `user/widget` | json}}\"}",
	}))
	page := mustRender(t, e, "page", nil)
	if page["w"].(map[string]any)["kind"] != "widget" {
		t.Fatalf("include 目标页未回退到短名称: %#v", page)
	}
}

// TestIncludeCycle 验证循环引用被深度上限拦截。
func TestIncludeCycle(t *testing.T) {
	e := New("", newFS(map[string]string{
		"a": "{\"self\":\"{{include `a`}}\"}",
	}))
	if _, err := e.Render(context.Background(), "a", nil); err == nil {
		t.Fatal("自引用 include 应报错")
	}
}

// TestIncludeOutsideRender 验证 include 不能在 Render 之外调用。
func TestIncludeOutsideRender(t *testing.T) {
	e := New("", newFS(map[string]string{}))
	if _, err := e.includeFunc("a", map[string]any{}); err == nil {
		t.Fatal("Render 之外调用 include 应报错")
	}
}

// TestIncludeBadArgs 验证 include 参数校验。
func TestIncludeBadArgs(t *testing.T) {
	e := New("", newFS(map[string]string{}))
	pushRenderState(context.Background())
	defer popRenderState()
	for _, args := range [][]any{
		{},                     // 无参数
		{42, map[string]any{}}, // sign 非字符串
		{"a", "not-a-map"},     // data 非对象
	} {
		if _, err := e.includeFunc(args...); err == nil {
			t.Fatalf("include(%v) 应报错", args)
		}
	}
}

// TestLayoutMode 布局模式：布局页动态 include 子页（参考 gview include 方式）。
func TestLayoutMode(t *testing.T) {
	e := New("", newFS(map[string]string{
		"layout/main":   "{\"type\":\"page\",\"header\":\"{{include `layout/header` . | json}}\",\"body\":\"{{include .mainTpl . | json}}\",\"user\":\"{{.userId | json}}\"}",
		"layout/header": `{"title":"{{.headerTitle | json}}"}`,
		"layout/footer": `{"title":"FOOTER"}`,
		"user/list":     `{"type":"list","rows":3}`,
	}))
	page := mustRender(t, e, "layout/main", map[string]any{
		"mainTpl":     "user/list",
		"userId":      7,
		"headerTitle": "HEADER 7",
	})
	if page["header"].(map[string]any)["title"] != "HEADER 7" {
		t.Fatalf("布局 header 未共享 data: %#v", page["header"])
	}
	if page["body"].(map[string]any)["rows"] != json.Number("3") {
		t.Fatalf("布局 body 未正确注入子页: %#v", page["body"])
	}
	if page["user"] != json.Number("7") {
		t.Fatalf("布局自身特征码未渲染: %#v", page["user"])
	}
}

// TestNestedLayout 验证嵌套组合：子页可继续 include 其他页面。
func TestNestedLayout(t *testing.T) {
	e := New("", newFS(map[string]string{
		"a": "{\"inA\":\"{{include `b` . | json}}\",\"v\":\"{{.v | json}}\"}",
		"b": "{\"inB\":\"{{include `c` . | json}}\"}",
		"c": `{"leaf":"{{.v | json}}"}`,
	}))
	page := mustRender(t, e, "a", map[string]any{"v": 9})
	if page["inA"].(map[string]any)["inB"].(map[string]any)["leaf"] != json.Number("9") {
		t.Fatalf("嵌套 include 未正确传递 data: %#v", page)
	}
}

// ---- ErrNotFound 哨兵错误 / fail-fast 查找 ----

// TestErrNotFoundSentinel 验证「无此页面」可经 errors.Is 与哨兵错误匹配，
// 而「页面存在但坏了」不匹配哨兵（调用方据此区分 404 与 500）。
func TestErrNotFoundSentinel(t *testing.T) {
	e := New("", newFS(map[string]string{
		"broken": `{"a":`, // 存在但非法
	}))

	_, err := e.Render(context.Background(), "missing", nil)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("未命中应包装 ErrNotFound, got %v", err)
	}

	_, err = e.Render(context.Background(), "broken", nil)
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("坏页面不应匹配 ErrNotFound, got %v", err)
	}
}

// brokenFS 模拟「文件存在但读取失败（非不存在错误）」的来源：
// 对 fail 文件返回权限类错误，对其余文件返回 ErrNotExist。
type brokenFS struct {
	fail string
}

func (f brokenFS) Open(name string) (fs.File, error) {
	if name == f.fail {
		return nil, errors.New("access is denied")
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// TestLookupFailFast 验证来源中文件存在但读取失败时报错携带来源标签，
// 不静默跳过到下一来源（陈旧副本顶上即排查地狱）。
func TestLookupFailFast(t *testing.T) {
	e := New("", brokenFS{fail: "user/list.json"})
	e.RegisterFS(newFS(map[string]string{"user/list": `{"from":"fallback"}`}))

	_, err := e.Render(context.Background(), "user/list", nil)
	if err == nil {
		t.Fatal("读取失败应报错而非静默使用下一来源")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("读取失败不应归类为「无此页面」: %v", err)
	}
	if !strings.Contains(err.Error(), "fs0") {
		t.Fatalf("错误应携带来源标签: %v", err)
	}

	// 缺失文件返回 ErrNotExist 的来源应正常跳过（不触发 fail-fast）。
	e2 := New("", brokenFS{fail: "other.json"})
	e2.RegisterFS(newFS(map[string]string{"user/list": `{"from":"ok"}`}))
	page := mustRender(t, e2, "user/list", nil)
	if page["from"] != "ok" {
		t.Fatalf("ErrNotExist 应跳过继续查找: %#v", page)
	}
}

// ---- Strict 严格寻址 ----

// TestStrictDisablesFallback 验证严格模式禁用同名层级回退，
// 只按完整路径精确命中。
func TestStrictDisablesFallback(t *testing.T) {
	e := New("", newFS(map[string]string{
		"list": `{"from":"short"}`,
	})).Strict()

	_, err := e.Render(context.Background(), "user/list", nil)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("严格模式下 user/list 不应回退到 list.json, got %v", err)
	}

	// 完整路径仍正常命中。
	e2 := New("", newFS(map[string]string{
		"user/list": `{"from":"full"}`,
		"list":      `{"from":"short"}`,
	})).Strict()
	page := mustRender(t, e2, "user/list", nil)
	if page["from"] != "full" {
		t.Fatalf("严格模式完整路径应精确命中: %#v", page)
	}
}

// ---- Funcs 自定义模板函数 ----

// TestFuncsCustomFunc 验证调用方注册的模板函数可在特征码中调用。
func TestFuncsCustomFunc(t *testing.T) {
	e := New("", newFS(map[string]string{
		"page": `{"title":"{{T ` + "`标题`" + ` | json}}"}`,
	})).Funcs(template.FuncMap{
		"T": func(s string) string { return "[i18n]" + s },
	})

	page := mustRender(t, e, "page", nil)
	if page["title"] != "[i18n]标题" {
		t.Fatalf("自定义函数未生效: %#v", page)
	}
}

// TestFuncsBuiltinsWin 验证与内置函数同名时以内置函数为准。
func TestFuncsBuiltinsWin(t *testing.T) {
	e := New("", newFS(map[string]string{
		"page": `{"v":"{{.v | json}}"}`,
	})).Funcs(template.FuncMap{
		"json": func(v any) string { return "hijacked" },
	})

	page := mustRender(t, e, "page", map[string]any{"v": 1})
	if page["v"] != json.Number("1") {
		t.Fatalf("内置 json 函数不应被覆盖: %#v", page)
	}
}

// ---- Validate 启动期全量校验 ----

// TestValidateAllPages 验证全部页面合法时返回 nil。
func TestValidateAllPages(t *testing.T) {
	e := New("", newFS(map[string]string{
		"a":       `{"v":1}`,
		"user/b":  `{"v":"{{.v | json}}"}`,
		"deep/c":  "{\"part\":\"{{include `a` | json}}\"}",
	}))
	if err := e.Validate(); err != nil {
		t.Fatalf("合法页面不应报错: %v", err)
	}
}

// TestValidateCatchesBrokenPage 验证坏页面（JSON 或模板语法错误）被校验拦截
// 且错误包含 sign。
func TestValidateCatchesBrokenPage(t *testing.T) {
	for _, broken := range []string{
		`{"a":`,               // 非法 JSON
		`{"a":"{{.x | json}}"`, // 渲染后引号缺失 → 非法 JSON
		`{"a":"{{end}}"}`,      // 模板语法错误
	} {
		e := New("", newFS(map[string]string{"a": `{"v":1}`, "bad": broken}))
		err := e.Validate()
		if err == nil {
			t.Fatalf("坏页面应使校验失败: %s", broken)
		}
		if !strings.Contains(err.Error(), `"bad"`) {
			t.Fatalf("错误应包含页面 sign: %v", err)
		}
	}
}

// TestValidateRespectsSourcePriority 验证同一 sign 只校验优先级最高的来源
//（低优先级来源中被遮蔽的同名页面不参与校验）。
func TestValidateRespectsSourcePriority(t *testing.T) {
	root := t.TempDir()
	// 根目录的 a.json 遮蔽 fs 中的坏副本。
	if err := os.WriteFile(filepath.Join(root, "a.json"), []byte(`{"from":"local"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	e := New(root, newFS(map[string]string{"a": `{"bad":`}))
	if err := e.Validate(); err != nil {
		t.Fatalf("被遮蔽的坏副本不应导致校验失败: %v", err)
	}

	// 根目录自身坏文件必须被校验出。
	root2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(root2, "b.json"), []byte(`{"bad":`), 0o644); err != nil {
		t.Fatal(err)
	}
	e2 := New(root2)
	err := e2.Validate()
	if err == nil || !strings.Contains(err.Error(), `"b"`) {
		t.Fatalf("根目录坏文件应使校验失败: %v", err)
	}
}

// TestValidateSkipsNonJSON 验证非 .json 文件不参与校验。
func TestValidateSkipsNonJSON(t *testing.T) {
	m := fstest.MapFS{
		"a.json":     &fstest.MapFile{Data: []byte(`{"v":1}`)},
		"README.md":  &fstest.MapFile{Data: []byte(`这不是 JSON`)},
		"notes.jsonx": &fstest.MapFile{Data: []byte(`也不是 JSON`)},
	}
	if err := New("", m).Validate(); err != nil {
		t.Fatalf("非 .json 文件不应参与校验: %v", err)
	}
}
