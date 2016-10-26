package generator

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/types"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"text/template"

	"golang.org/x/tools/go/loader"
	"golang.org/x/tools/imports"
)

//Generator stores information about imports, loaded packages,
//registered template functions and global template variables
type Generator struct {
	*types.Info
	loader.Config

	imports         map[string]*importInfo
	header          string
	packageName     string
	body            *bytes.Buffer
	templateFuncs   map[string]interface{}
	typeConversions map[string]string
	vars            map[string]string
	loadedPackages  map[string]*loader.PackageInfo
}

type importInfo struct {
	Alias *string
	Name  string
}

//String returns package selector (alias)
func (i importInfo) String() string {
	if i.Alias != nil {
		return *i.Alias
	}

	return i.Name
}

var chanPrefixes = map[types.ChanDir]string{
	types.SendOnly: "chan<- ",
	types.RecvOnly: "<-chan ",
	types.SendRecv: "chan ",
}

//New creates new Generator and returns pointer to it
func New() *Generator {
	gen := &Generator{
		header: `
			This code was automatically generated using github.com/gojuno/generator lib.
			Please DO NOT modify.
		`,
		imports:         map[string]*importInfo{},
		templateFuncs:   map[string]interface{}{},
		typeConversions: map[string]string{},
		vars:            map[string]string{},
		loadedPackages:  map[string]*loader.PackageInfo{},
		body:            bytes.NewBuffer([]byte{}),
	}

	gen.AddTemplateFunc("signature", gen.FuncSignature)
	gen.AddTemplateFunc("params", gen.FuncParams)
	gen.AddTemplateFunc("results", gen.FuncResults)
	gen.AddTemplateFunc("new", gen.New)
	gen.AddTemplateFunc("typeOf", gen.TypeOf)
	gen.AddTemplateFunc("first", First)
	gen.AddTemplateFunc("last", Last)
	gen.AddTemplateFunc("mixedCaps", MixedCaps)

	return gen
}

// SetHeader sets header comment for generated files
func (g *Generator) SetHeader(h string) {
	g.header = h
}

//Import imports package and returns its path and selector(alias)
func (g *Generator) Import(pkg interface{}) (path, selector string) {
	path, name := g.PackagePathAndName(pkg)

	imp, ok := g.imports[path]
	if !ok {
		imp = &importInfo{Name: name}

		if g.isSelectorTaken(path, name) {
			imp.Alias = g.makeAliasForName(name)
		}

		g.imports[path] = imp
	}

	return path, imp.String()
}

//isSelectorTaken checks if given selector was taken by some other package
func (g *Generator) isSelectorTaken(path, selector string) bool {
	for p, imp := range g.imports {
		if imp.String() == selector && p != path {
			return true
		}
	}

	return false
}

//makeAliasForName checks selectors of all imported packages and gives
//selector with numeric suffix if there's any packages' selectors that starts
//with the same name
func (g *Generator) makeAliasForName(name string) *string {
	var prefixesCount int
	for _, imp := range g.imports {
		if strings.HasPrefix(imp.String(), name) {
			prefixesCount++
		}
	}

	alias := fmt.Sprintf("%s%d", name, prefixesCount+1)

	return &alias
}

//PackagePathAndName takes package information that can be one of:
//string, *types.Package, *ast.ImportSpec and returns it's import path and
//name
func (g *Generator) PackagePathAndName(pkg interface{}) (path string, name string) {
	switch p := pkg.(type) {
	case string:
		path = p
		pkgInfo, err := g.loadPackage(path)
		if err != nil { //extract name from the package path itself
			log.Fatalf("failed to import package %q: %v", path, err)
		}

		name = pkgInfo.Pkg.Name()
	case *types.Package:
		path = p.Path()
		name = p.Name()
	case *ast.ImportSpec:
		return g.PackagePathAndName(p.Path.Value[1 : len(p.Path.Value)-1])
	default:
		log.Fatalf("unsupported pkg type: %T", pkg)
	}

	return path, name
}

//loadPackage loads package by it's path caches and returns package information
func (g *Generator) loadPackage(path string) (*loader.PackageInfo, error) {
	if _, ok := g.loadedPackages[path]; !ok {
		config := g.Config //using base config settings
		config.Import(path)

		prog, err := config.Load()
		if err != nil {
			return nil, fmt.Errorf("failed to load package %q: %v", path, err)
		}

		g.loadedPackages[path] = prog.Package(path)
	}

	return g.loadedPackages[path], nil
}

//ImportWithAlias places given package to the list of imported packages and assigns alias to it.
//It can be useful when you want to use certain alias for the package and give generated aliases for
//other packages that might have the same name as the given one.
func (g *Generator) ImportWithAlias(pkg interface{}, alias string) (path string, err error) {
	path, _ = g.Import(pkg)

	for p, i := range g.imports {
		if i.Alias != nil {
			if p == path && *i.Alias != alias {
				return "", fmt.Errorf("can't set an alias %q for package %q, alias is already set: %q", alias, path, i.Alias)
			}

			if p != path && *i.Alias == alias {
				return "", fmt.Errorf("can't set an alias %q for package %q, alias is already used for: %q", alias, path, p)
			}
		}
	}

	g.imports[path].Alias = &alias

	return path, nil
}

//WriteToFilename write generated source to the file and performs
//formatting of the source with goimports (which is also doing gofmt internally)
func (g *Generator) WriteToFilename(filename string) error {
	if g.packageName == "" {
		g.packageName = filepath.Base(filepath.Dir(filename))
	}

	buf := bytes.NewBuffer([]byte{})

	if _, err := g.WriteTo(buf); err != nil {
		return fmt.Errorf("failed to write generated source to file: %v", err)
	}

	formatted, err := imports.Process(filename, buf.Bytes(), nil)
	if err != nil {
		return fmt.Errorf("failed to fix imports: %v\n%s", err, string(buf.Bytes()))
	}

	f, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("failed to create file: %v", err)
	}

	if _, err := f.Write(formatted); err != nil {
		return fmt.Errorf("failed to write formatted source: %v", err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("failed to close file: %v", err)
	}

	return nil
}

//WriteTo generates source and writes result to w
func (g *Generator) WriteTo(w io.Writer) (int64, error) {
	buf := bytes.NewBuffer([]byte{})

	if err := g.writeHeader(buf); err != nil {
		return 0, fmt.Errorf("failed to write file header: %v", err)
	}

	if _, err := fmt.Fprintf(buf, "\npackage %s\n", g.packageName); err != nil {
		return 0, fmt.Errorf("failed to write package name: %v", err)
	}

	if err := g.writeImports(buf); err != nil {
		return 0, fmt.Errorf("failed to write imports: %v", err)
	}

	if _, err := buf.Write(g.body.Bytes()); err != nil {
		return 0, fmt.Errorf("failed to write body: %v", err)
	}

	i, err := w.Write(buf.Bytes())
	return int64(i), err
}

//writeHeader writes header comment to w
func (g *Generator) writeHeader(w io.Writer) error {
	header := append([]byte("/*\n"), []byte(strings.Trim(g.header, " \t\n\r"))...)
	header = append(header, []byte("\n*/")...)

	_, err := w.Write(header)
	return err
}

//writeImports generates import instructions for all imported packages
func (g *Generator) writeImports(w io.Writer) error {
	if len(g.imports) == 0 {
		return nil
	}

	if _, err := fmt.Fprintf(w, "\nimport (\n"); err != nil {
		return err
	}

	for path, alias := range g.imports {
		if _, err := fmt.Fprintf(w, "%s %q\n", alias, path); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprintf(w, ")\n"); err != nil {
		return err
	}

	_, err := w.Write([]byte{})
	return err
}

//Write implements io.Writer. Performs writing of the byte slice to the
//generated source body
func (g *Generator) Write(b []byte) (int, error) {
	return g.body.Write(b)
}

//TypeOf returns a type name for the given argument with package selector.
//Package selector is generated with respect to packages imported with aliases
//and automatically generated aliases. Input argument type can be one of:
//ast.Expr, *Param, types.TypeAndValue, types.Type
func (g *Generator) TypeOf(i interface{}) string {
	switch t := i.(type) {
	case ast.Expr:
		typesType, ok := g.Types[t]
		if !ok {
			log.Fatalf("can't detect type of expression %+v", typesType)
		}

		return g.TypeOf(typesType)
	case *Param:
		return t.Type
	case types.TypeAndValue:
		return g.TypeOf(t.Type)
	case types.Type:
		return g.typeOf(t)
	default:
		log.Fatalf("unsupported argument type %T for TypeName", i)
	}

	return ""
}

//typeOf returns type name with package selector for the given type
func (g *Generator) typeOf(tt types.Type) string {
	switch t := tt.(type) {
	case *types.Chan:
		return chanPrefixes[t.Dir()] + g.typeOf(t.Elem())
	case *types.Pointer:
		return "*" + g.typeOf(t.Elem())
	case *types.Array:
		return fmt.Sprintf("[%d]%s", t.Len(), g.typeOf(t.Elem()))
	case *types.Slice:
		return "[]" + g.typeOf(t.Elem())
	case *types.Map:
		return "map[" + g.typeOf(t.Key()) + "]" + g.typeOf(t.Elem())
	case *types.Struct:
		name := "struct{\n"
		for i := 0; i < t.NumFields(); i++ {
			field := t.Field(i)
			name += field.Name() + " " + g.typeOf(field.Type()) + "\n"
		}
		return name + "\n}"
	case *types.Signature:
		signature, err := g.FuncSignature(t)
		if err != nil {
			log.Fatalf("failed to get func signature: %v", err)
		}

		return "func" + signature
	case *types.Named:
		fullName := t.Obj().Type().String()
		if convertTo, ok := g.typeConversions[fullName]; ok {
			fullName = convertTo
		}

		i := strings.LastIndex(fullName, ".")
		if i == -1 {
			return fullName
		}

		pkgPath := fullName[0:i]
		name := fullName[i+1:]

		selector := g.PackageSelector(pkgPath)
		if selector == "" {
			return name
		}

		return selector + "." + name
	case *types.Basic:
		return t.Name()
	case *types.Interface:
		return "interface{}"
	default:
		log.Fatalf("Can't generate local type name for type %T: %v", t, t)
	}

	return ""
}

//PackageSelector returns package selector with respect to packages
//imported with aliases and automatically generated aliases
func (g *Generator) PackageSelector(pkg interface{}) string {
	_, selector := g.Import(pkg)
	return selector
}

//ProcessTemplate adds declarations of the global variables registered via SetVar.
//parses template, registers helper functions to be accessible from
//template and executes template
func (g *Generator) ProcessTemplate(tmplName, tmpl string, data interface{}) error {
	vars := []string{}
	for varName, varValue := range g.vars {
		vars = append(vars, fmt.Sprintf("{{ $%s := %q }}", varName, varValue))
	}

	if len(vars) > 0 {
		tmpl = strings.Join(vars, "\n") + "\n" + tmpl
	}

	t, err := template.New(tmplName).Funcs(g.templateFuncs).Parse(tmpl)
	if err != nil {
		return fmt.Errorf("failed to parse template: %v\n%s", err, tmpl)
	}

	if err := t.Execute(g.body, data); err != nil {
		return fmt.Errorf("failed to execute template: %v", err)
	}

	return nil
}

//AddTemplateFunc registers a helper function that can be called from any of
//templates that processed via generator
func (g *Generator) AddTemplateFunc(name string, f interface{}) {
	g.templateFuncs[name] = f
}

//FuncSignature returns a signature of the function represented by f
//f can be one of: ast.Expr, ast.SelectorExpr, types.Type, types.Signature
func (g *Generator) FuncSignature(f interface{}) (string, error) {
	params, err := g.FuncParams(f)
	if err != nil {
		return "", fmt.Errorf("failed to get %+v func params: %v", f, err)
	}

	results, err := g.FuncResults(f)
	if err != nil {
		return "", fmt.Errorf("failed to get %+v func results: %v", f, err)
	}

	return fmt.Sprintf("(%s) (%s)", params, results), nil
}

//Param is a proxy struct that represents parameter name and it's type
type Param struct {
	Type         string
	Name         string
	OriginalType types.Type
}

func (p *Param) String() string {
	return fmt.Sprintf("%s %s", p.Name, p.Type)
}

//ParamSet is a helper structure that represents list of input or result parameters
//of a function
type ParamSet []*Param

func (ps ParamSet) String() string {
	params := make([]string, len(ps))
	for i := 0; i < len(ps); i++ {
		params[i] = ps[i].String()
	}

	return strings.Join(params, ",")
}

//Names returns a list of params names from the ParamSet separated by
//commas
func (ps ParamSet) Names() string {
	names := make([]string, len(ps))
	for i := 0; i < len(ps); i++ {
		names[i] = ps[i].Name
	}

	return strings.Join(names, ",")

}

//FuncParams returns a slice of function parameters
func (g *Generator) FuncParams(f interface{}) (ParamSet, error) {
	signature, err := g.funcSignature(f)
	if err != nil {
		return nil, fmt.Errorf("failed to get func %+v signature", f)
	}

	return g.makeParamSet("p", signature.Params()), nil
}

//FuncResults returns a slice of function results
func (g *Generator) FuncResults(f interface{}) (ParamSet, error) {
	signature, err := g.funcSignature(f)
	if err != nil {
		return nil, fmt.Errorf("failed to get func %+v signature", f)
	}

	return g.makeParamSet("r", signature.Results()), nil
}

//funcSignature returns *types.Signature related to f. Where f can be one
//of: ast.Expr, *ast.SelectorExpr, types.Type, *types.Signature
func (g *Generator) funcSignature(f interface{}) (*types.Signature, error) {
	switch t := f.(type) {
	case ast.Expr:
		if tt, ok := g.Info.Types[t]; ok {
			if s, ok := tt.Type.(*types.Signature); ok {
				return s, nil
			}
		}

		return nil, fmt.Errorf("value %+v doesn't represent a function signature", t)
		//TODO: case *ast.FuncDecl:
	case *ast.SelectorExpr:
		selection, ok := g.Info.Selections[t]
		if !ok {
			return nil, fmt.Errorf("failed to find selection information for: %+v", f)
		}

		if selection.Kind() != types.MethodVal {
			log.Fatalf("given selection is not a method selector: %v", f)
		}

		return selection.Type().(*types.Signature), nil
	case *types.Signature:
		return t, nil
	case types.Type:
		if s, ok := t.(*types.Signature); ok {
			return s, nil
		}

		return nil, fmt.Errorf("value %+v doesn't represent a function signature", t)
	}

	return nil, fmt.Errorf("unsupported type of the argument: %T (%+v)", f, f)
}

// makeParamSet takes a tuple of func params/results and returns ParamSet
// with types conversions applied and params names renamed according to prefix, i.e.
// if prefix is "p" then first param name will be p0. If prefix is "" no renaming
// is done.
func (g *Generator) makeParamSet(prefix string, params *types.Tuple) ParamSet {
	ps := make(ParamSet, params.Len())

	for i := 0; i < params.Len(); i++ {
		param := params.At(i)
		name := param.Name()

		if prefix != "" {
			name = fmt.Sprintf("%s%d", prefix, i)
		}

		ps[i] = &Param{
			Name:         name,
			Type:         g.typeOf(param.Type()),
			OriginalType: param.Type(),
		}
	}

	return ps
}

//ConvertType sets type conversion rule that will be applied by generator to
//functions' arguments types structs' fields types and so on.
func (g *Generator) ConvertType(from, to string) {
	g.typeConversions[from] = to
}

//New returns initialization string of the variable according to it's type,
//i.e. new(..) for pointer types, make(...) for maps/chans and slices,
//var for arrays and named types, etc
func (g *Generator) New(varName string, varType interface{}) (string, error) {
	switch t := varType.(type) {
	case *Param:
		return g.New(varName, t.OriginalType)
	case types.Type:
		return g.varInitString(varName, t)
	}

	return "", fmt.Errorf("New: unsupported argument type: %T", varType)
}

func (g *Generator) varInitString(varName string, varType types.Type) (string, error) {
	switch t := varType.(type) {
	case *types.Chan:
		return fmt.Sprintf("%s := make(%s)", varName, g.TypeOf(t.Elem())), nil
	case *types.Map:
		return fmt.Sprintf("%s := make(map[%s]%s)", varName, g.TypeOf(t.Key()), g.TypeOf(t.Elem())), nil
	case *types.Slice:
		return fmt.Sprintf("%s := make(%s)", varName, g.TypeOf(t.Elem())), nil
	case *types.Array:
		return fmt.Sprintf("var %s [%d]%s", varName, t.Len(), g.TypeOf(t.Elem())), nil
	case *types.Pointer:
		return fmt.Sprintf("%s := new(%s)", varName, g.TypeOf(t.Elem())), nil
	case *types.Struct:
		return fmt.Sprintf("%s := %s{}", varName, g.TypeOf(t)), nil
	case *types.Signature:
		return fmt.Sprintf("%s := func%s{}", varName, g.TypeOf(t)), nil
	case *types.Named:
		return fmt.Sprintf("var %s %s", varName, g.TypeOf(t)), nil
	}

	return "", fmt.Errorf("varInitString: unsupported argument type %T", varType)
}

//SetPackageName sets package name of the generated file
func (g *Generator) SetPackageName(name string) {
	g.packageName = name
	g.SetVar("packageName", name)
}

//SetVar sets global variable that will be accessible from all templates
//processed with generator
func (g *Generator) SetVar(varName, varValue string) {
	g.vars[varName] = varValue
}

//PackageAbsPath parses GOPATH environment variable and returns a first
//absolute path matched the package p import path
func PackageAbsPath(p interface{}) (string, error) {
	switch t := p.(type) {
	case *types.Package:
		return PackageAbsPath(t.Path())
	case *ast.ImportSpec:
		return PackageAbsPath(t.Path.Value[1 : len(t.Path.Value)-1])
	case string:
		gopath := os.Getenv("GOPATH")
		for _, path := range filepath.SplitList(gopath) {
			fullPath := filepath.Join(path, "src", t)
			absPath, err := filepath.Abs(fullPath)
			if err != nil {
				continue
			}

			if _, err = os.Stat(fullPath); err != nil {
				continue
			}

			return absPath, nil
		}

		return "", fmt.Errorf("failed to find %q in GOPATH (%s)", t, gopath)
	}

	return "", fmt.Errorf("unsupported argument type for PackageFullPath: %T", p)
}

func PackageOf(filePath string) (string, error) {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to get fiel %q absolute path: %v", filePath, err)
	}

	f, err := os.Open(absPath)
	if err != nil {
		return "", fmt.Errorf("failed to open %q: %v", absPath, err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("failed to get file information for %q: %v", absPath, err)
	}

	path := absPath
	if fi.Mode().IsRegular() {
		if !strings.HasSuffix(path, ".go") {
			return "", fmt.Errorf("%q is not a *.go file", absPath)
		}

		path = filepath.Dir(absPath)
	}

	gopath := os.Getenv("GOPATH")
	for _, gp := range filepath.SplitList(gopath) {
		gp, err = filepath.Abs(gp)
		if err != nil {
			continue
		}

		gp += "/src/"

		if strings.HasPrefix(path, gp) {
			return strings.Replace(path, gp, "", 1), nil
		}
	}

	return "", fmt.Errorf("can't detect package for file: %q", filePath)
}

//Copy copies entities sources to the generated body applying
//type conversion rules and correct packages aliases
func (g *Generator) Copy(n interface{}) error {
	switch v := n.(type) {
	case *ast.TypeSpec:
		return g.CopyType(v)
	}

	return fmt.Errorf("can't copy variable of type: %T", n)
}

//CopyType copies type declaration to the generated body
func (g *Generator) CopyType(typeSpec *ast.TypeSpec) error {
	switch t := typeSpec.Type.(type) {
	case *ast.StructType, *ast.MapType, *ast.ChanType, *ast.FuncType, *ast.ArrayType:
		fmt.Fprintf(g, "type %s %s\n", typeSpec.Name.Name, g.TypeOf(typeSpec.Type))
	case *ast.Ident:
		fmt.Fprintf(g, "type %s %s\n", typeSpec.Name.Name, t.Name)
	default:
		return fmt.Errorf("can't copy item of type %T", t)
	}

	return nil
}

//MixedCaps transform underscored string to mixed-caps string
func MixedCaps(s string) string {
	chunks := strings.Split(s, "_")
	for i, chunk := range chunks {
		chunks[i] = strings.Title(chunk)
	}

	return strings.Join(chunks, "")
}

//Last returns last element of given slice or interface
func Last(i interface{}) (interface{}, error) {
	t := reflect.TypeOf(i)
	v := reflect.ValueOf(i)

	switch t.Kind() {
	case reflect.Array, reflect.Slice:
	default:
		return nil, fmt.Errorf("value is nither Slice nor Array")
	}

	if v.Len() == 0 {
		return nil, fmt.Errorf("empty array")
	}

	return v.Index(v.Len() - 1).Interface(), nil
}

//First returns first element of given slice or interface
func First(i interface{}) (interface{}, error) {
	t := reflect.TypeOf(i)
	v := reflect.ValueOf(i)

	switch t.Kind() {
	case reflect.Array, reflect.Slice:
	default:
		return nil, fmt.Errorf("value is nither Slice nor Array")
	}

	if v.Len() == 0 {
		return nil, fmt.Errorf("empty array")
	}

	return v.Index(0).Interface(), nil
}