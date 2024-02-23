package main

import (
	"bytes"
	"crypto/sha512"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"text/template"

	"mvdan.cc/gofumpt/format"
)

const (
	importRisorObject = "github.com/risor-io/risor/object"
)

type Options struct {
	Modules     string
	IgnoreFiles string
}

func main() {
	var options Options
	flag.StringVar(&options.Modules, "modules", "modules", `Path to directory of modules`)
	flag.StringVar(&options.IgnoreFiles, "ignore", ".*_stub.go$", `Regex of files to ignore.`)
	flag.Parse()

	if err := run(options); err != nil {
		fmt.Printf("ERROR: %s\n", err)
		os.Exit(1)
	}
}

func run(options Options) error {
	entries, err := os.ReadDir(options.Modules)
	if err != nil {
		return err
	}
	fmt.Println("Generating Risor module bindings")
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(options.Modules, entry.Name())
		if err := runInDir(path, options); err != nil {
			return fmt.Errorf("module %q: %w", path, err)
		}
	}
	return nil
}

func runInDir(dir string, options Options) error {
	fset := token.NewFileSet()
	ignoreRegex, err := regexp.Compile(options.IgnoreFiles)
	if err != nil {
		return fmt.Errorf("parse -ignore flag: %w", err)
	}
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_gen.go") &&
			!strings.HasSuffix(fi.Name(), "_test.go") &&
			!strings.HasSuffix(fi.Name(), "_example.go") &&
			!ignoreRegex.MatchString(fi.Name())
	}, parser.ParseComments|parser.DeclarationErrors)
	if err != nil {
		return err
	}

	if len(pkgs) != 1 {
		var pkgNames []string
		for name := range pkgs {
			pkgNames = append(pkgNames, name)
		}
		return fmt.Errorf("directory must only contain 1 package, but got: %s", pkgNames)
	}

	for _, pkg := range pkgs {
		mod, err := Parse(fset, pkg)
		if err != nil {
			return err
		}

		if !mod.HasGenerateComment {
			fmt.Printf("Skipping, missing risor:generate comment: %s\n", dir)
			continue
		}

		genFile := filepath.Join(dir, mod.Name+"_gen.go")

		changed, written, err := mod.WriteFile(genFile, options)
		if err != nil {
			return fmt.Errorf("write generated file: %w", err)
		}

		if !changed {
			fmt.Printf("No changes to file: %s\n", genFile)
			continue
		}

		fmt.Printf("Wrote to file: %s (%d B)\n", genFile, written)
	}
	return nil
}

type Module struct {
	Name string

	HasGenerateComment bool
	skipModulesFunc    bool

	fset             *token.FileSet
	buildConstraints []string
	exportedFuncs    []ExportedFunc
	imports          []string
}

func Parse(fset *token.FileSet, pkg *ast.Package) (*Module, error) {
	mod := &Module{
		Name: pkg.Name,
		fset: fset,
	}
	for path, file := range pkg.Files {
		if err := mod.parseFile(file); err != nil {
			return nil, fmt.Errorf("file %q: %w", path, err)
		}
	}
	return mod, nil
}

func (m *Module) WriteFile(path string, options Options) (bool, int, error) {
	var buf bytes.Buffer
	m.Fprint(&buf, options)

	fmtOpts := format.Options{
		ModulePath: filepath.Dir(path),
	}

	if dbg, ok := debug.ReadBuildInfo(); ok {
		// turn "go1.19.2" into "1.19.2"
		fmtOpts.LangVersion = strings.TrimPrefix(dbg.GoVersion, "go")
	}

	// Format using gofumpt
	b, err := format.Source(buf.Bytes(), fmtOpts)
	if err != nil {
		return false, 0, fmt.Errorf("format file %q: %w", path, err)
	}

	return writeFileCheckChanged(path, b)
}

func writeFileCheckChanged(path string, b []byte) (bool, int, error) {
	// Don't truncate on open, because we also want to read the file
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o666)
	if err != nil {
		return false, 0, err
	}
	defer f.Close()

	if calcHash(f) == calcHash(bytes.NewReader(b)) {
		return false, 0, nil // no change
	}

	// Reset cursor after read
	f.Seek(0, io.SeekStart)

	written, err := f.Write(b)
	if err != nil {
		return false, 0, err
	}
	// Must manually truncate the file in case the file got smaller
	if err := f.Truncate(int64(written)); err != nil {
		return false, 0, err
	}

	return true, written, nil
}

func calcHash(r io.Reader) string {
	hash := sha512.New()
	if _, err := io.Copy(hash, r); err != nil {
		return ""
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

var tmpl = template.Must(template.New("generated").Parse(`// Code generated by risor-modgen. DO NOT EDIT.
{{- if .BuildConstraints }}
{{ range .BuildConstraints }}
{{ . }}
{{- end }}
{{- end }}

package {{ .Package }}

{{- with .Imports }}

import (
	{{- range . }}
	"{{ . }}"
	{{- end }}
)
{{- end }}

{{- if .ExportedFuncs }}
{{- range $func := .ExportedFuncs }}

// {{ .FuncGenName }} is a wrapper function around [{{ .FuncName }}]
// that implements [object.BuiltinFunction].
func {{ .FuncGenName }}(ctx context.Context, args ...object.Object) object.Object {
	if len(args) != {{ len .Params }} {
		return object.NewArgsError("{{ $.Package }}.{{ .ExportedName }}", {{ len .Params }}, len(args))
	}
	{{- range $index, $param := .Params }}
	{{- if .ReadFunc }}
	{{ .Name }}Param{{ if .CastFunc }}Raw{{ end }}, err := object.{{ .ReadFunc }}(args[{{ $index }}])
	if err != nil {
		return err
	}
	{{- else }}
	{{ .Name }}Param{{ if .CastFunc }}Raw{{ end }} := args[{{ $index }}]
	{{- end }}
	{{- if .CastFunc }}
	{{- if .CastMaxValue }}
	if {{ .Name }}ParamRaw > {{ .CastMaxValue }} {
		return object.Errorf("type error: {{ $.Package }}.{{ $func.ExportedName }} argument '{{ .Name }}' (index {{ $index }}) cannot be > %v", {{ .CastMaxValue }})
	}
	{{- end }}
	{{- if .CastMinValue }}
	if {{ .Name }}ParamRaw < {{ .CastMinValue }} {
		return object.Errorf("type error: {{ $.Package }}.{{ $func.ExportedName }} argument '{{ .Name }}' (index {{ $index }}) cannot be < %v", {{ .CastMinValue }})
	}
	{{- end }}
	{{ .Name }}Param := {{ .CastFunc }}({{ .Name }}ParamRaw)
	{{- end }}
	{{- end }}
	{{- if or .Return .ReturnsError }}
	{{ if .Return }}result{{ end -}}
	{{- if and .Return .ReturnsError }}, {{ end -}}
	{{ if .ReturnsError }}resultErr{{ end }} := {{ end -}}
	{{ .FuncName }}(
		{{- if .NeedsContext -}}
		ctx{{ if .Params }}, {{ end }}
		{{- end -}}
		{{- range $index, $param := .Params -}}
			{{- if gt $index 0}}, {{ end -}}
			{{.Name}}Param
		{{- end -}}
	)
	{{- if .ReturnsError }}
	if resultErr != nil {
		return object.NewError(resultErr)
	}
	{{- end }}
	{{- if .Return }}
	return {{ with .Return.NewFunc -}}object.{{ . }}({{ end }}
		{{- with .Return.CastFunc -}}
			{{ . }}(result)
		{{- else -}}
			result
		{{- end -}}
	{{- if .Return.NewFunc }}){{ end }}
	{{- else }}
	return object.Nil
	{{- end }}
}
{{- end }}
{{- end }}

// addGeneratedBuiltins adds the generated builtin wrappers to the given map.
//
// Useful if you want to write your own "Module()" function.
func addGeneratedBuiltins(builtins map[string]object.Object) map[string]object.Object {
	{{- range .ExportedFuncs }}
	builtins["{{ .ExportedName }}"] = object.NewBuiltin("{{ $.Package }}.{{ .ExportedName }}", {{ .FuncGenName }})
	{{- end }}
	return builtins
}

{{ if not .SkipModulesFunc -}}
// The "Module()" function can be disabled with "//risor:generate no-module-func"

// Module returns the Risor module object with all the associated builtin
// functions.
func Module() *object.Module {
	return object.NewBuiltinsModule("{{ .Package }}", addGeneratedBuiltins(map[string]object.Object{}))
}
{{ end }}
`))

func (m *Module) Fprint(w io.Writer, options Options) {
	if !m.skipModulesFunc {
		m.addImport(importRisorObject)
	}

	slices.Sort(m.imports)

	tmpl.Execute(w, struct {
		Package          string
		Imports          []string
		BuildConstraints []string
		SkipModulesFunc  bool
		ExportedFuncs    []ExportedFunc
	}{
		Package:          m.Name,
		Imports:          m.imports,
		BuildConstraints: m.buildConstraints,
		SkipModulesFunc:  m.skipModulesFunc,
		ExportedFuncs:    m.exportedFuncs,
	})
}

func (m *Module) parseFile(file *ast.File) error {
	fileHasGenerateComment, err := m.parseGenerateComment(file)
	if err != nil {
		return err
	}

	if fileHasGenerateComment {
		// Only consider build constraints on file with //risor:generate
		if err := m.parseBuildConstraints(file); err != nil {
			return err
		}
	}

	for _, decl := range file.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			if err := m.parseFuncDecl(decl); err != nil {
				return fmt.Errorf("func %s: %w", decl.Name.Name, err)
			}
		case *ast.BadDecl:
		case *ast.GenDecl:
		}
	}
	return nil
}

func (m *Module) parseGenerateComment(file *ast.File) (bool, error) {
	for _, group := range file.Comments {
		for _, comment := range group.List {
			after, ok := cutPrefixAndSpace(comment.Text, "//risor:generate")
			if !ok {
				continue
			}

			if m.HasGenerateComment {
				return false, fmt.Errorf("multiple //risor:generate comments found")
			}

			m.HasGenerateComment = true

			fields := strings.Fields(after)
			for _, field := range fields {
				if field == "no-module-func" {
					m.skipModulesFunc = true
					continue
				}

				return false, fmt.Errorf("invalid //risor:generate field: %q", field)
			}

			return true, nil
		}
	}
	return false, nil
}

func (m *Module) parseBuildConstraints(file *ast.File) error {
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if strings.HasPrefix(comment.Text, "//go:build ") ||
				strings.HasPrefix(comment.Text, "// +build ") {
				m.buildConstraints = append(m.buildConstraints, comment.Text)
			}
		}
	}
	return nil
}

func (m *Module) addImport(pkg string) {
	if !slices.Contains(m.imports, pkg) {
		m.imports = append(m.imports, pkg)
	}
}

func (m *Module) sprintExpr(node any) string {
	var buf bytes.Buffer
	printer.Fprint(&buf, m.fset, node)
	return buf.String()
}

func cutPrefixAndSpace(s, prefix string) (after string, ok bool) {
	after, ok = strings.CutPrefix(s, prefix)
	if !ok {
		return s, false
	}
	if after == "" {
		return
	}
	if after[0] != ' ' {
		return s, false
	}
	return after[1:], true
}