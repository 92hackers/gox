package gox

import (
	"context"
	"go/ast"
	"go/token"
	"go/types"
	"log"
	"reflect"
	"strconv"
)

type LoadPkgsFunc = func(at *Package, importPkgs map[string]*PkgRef, pkgPaths ...string) int
type LoadNamedFunc = func(at *Package, typ *types.Named)

const (
	DbgFlagInstruction = 1 << iota
	DbgFlagImport
	DbgFlagMatch
	DbgFlagComments
	DbgFlagWriteFile
	DbgFlagSetDebug
	DbgFlagPersistCache
	DbgFlagAll = DbgFlagInstruction | DbgFlagImport | DbgFlagMatch |
		DbgFlagComments | DbgFlagWriteFile | DbgFlagSetDebug | DbgFlagPersistCache
)

var (
	debugInstr        bool
	debugMatch        bool
	debugImport       bool
	debugComments     bool
	debugWriteFile    bool
	debugPersistCache bool
)

func SetDebug(dbgFlags int) {
	debugInstr = (dbgFlags & DbgFlagInstruction) != 0
	debugImport = (dbgFlags & DbgFlagImport) != 0
	debugMatch = (dbgFlags & DbgFlagMatch) != 0
	debugComments = (dbgFlags & DbgFlagComments) != 0
	debugWriteFile = (dbgFlags & DbgFlagWriteFile) != 0
	debugPersistCache = (dbgFlags & DbgFlagPersistCache) != 0
	if (dbgFlags & DbgFlagSetDebug) != 0 {
		log.Printf("SetDebug: import=%v, match=%v, instr=%v\n", debugImport, debugMatch, debugInstr)
	}
}

// ----------------------------------------------------------------------------

type PkgImporter interface {
	Import(pkgPath string) *PkgRef
}

type NodeInterpreter interface {
	// Position gets position of a Pos.
	Position(p token.Pos) token.Position

	// LoadExpr is called to load an expr code and return its position.
	LoadExpr(expr ast.Node) (string, token.Position)

	// Caller is called to return the name of a function call.
	Caller(expr ast.Node) string
}

// Config type
type Config struct {
	// Context specifies the context for the load operation.
	// If the context is cancelled, the loader may stop early
	// and return an ErrCancelled error.
	// If Context is nil, the load cannot be cancelled.
	Context context.Context

	// Logf is the logger for the config.
	// If the user provides a logger, debug logging is enabled.
	// If the GOPACKAGESDEBUG environment variable is set to true,
	// but the logger is nil, default to log.Printf.
	Logf func(format string, args ...interface{})

	// Dir is the directory in which to run the build system's query tool
	// that provides information about the packages.
	// If Dir is empty, the tool is run in the current directory.
	Dir string

	// Env is the environment to use when invoking the build system's query tool.
	// If Env is nil, the current environment is used.
	// As in os/exec's Cmd, only the last value in the slice for
	// each environment key is used. To specify the setting of only
	// a few variables, append to the current environment, as in:
	//
	//	opt.Env = append(os.Environ(), "GOOS=plan9", "GOARCH=386")
	//
	Env []string

	// BuildFlags is a list of command-line flags to be passed through to
	// the build system's query tool.
	BuildFlags []string

	// Fset provides source position information for syntax trees and types.
	// If Fset is nil, Load will use a new fileset, but preserve Fset's value.
	Fset *token.FileSet

	// ParseFile is called to read and parse each file
	// when preparing a package's type-checked syntax tree.
	// It must be safe to call ParseFile simultaneously from multiple goroutines.
	// If ParseFile is nil, the loader will uses parser.ParseFile.
	//
	// ParseFile should parse the source from src and use filename only for
	// recording position information.
	//
	// An application may supply a custom implementation of ParseFile
	// to change the effective file contents or the behavior of the parser,
	// or to modify the syntax tree. For example, selectively eliminating
	// unwanted function bodies can significantly accelerate type checking.
	ParseFile func(fset *token.FileSet, filename string, src []byte) (*ast.File, error)

	// HandleErr is called to handle errors.
	HandleErr func(err error)

	// NodeInterpreter is to interpret an ast.Node.
	NodeInterpreter NodeInterpreter

	// LoadPkgs is called to load all import packages.
	LoadPkgs LoadPkgsFunc

	// LoadNamed is called to load a delay-loaded named type.
	LoadNamed LoadNamedFunc

	// Prefix is name prefix.
	Prefix string

	// NewBuiltin is to create the builin package.
	NewBuiltin func(pkg PkgImporter, prefix string, conf *Config) *types.Package

	// untyped bigint, untyped bigrat, untyped bigfloat
	UntypedBigInt, UntypedBigRat, UntypedBigFloat *types.Named
}

// ----------------------------------------------------------------------------

type file struct {
	decls         []ast.Decl
	importPkgs    map[string]*PkgRef
	allPkgPaths   []string // all import pkgPaths
	delayPkgPaths []string // all delay-load pkgPaths
	pkgBig        *PkgRef
	removedExprs  bool
}

func pkgPathNotFound(allPkgPaths []string, pkgPath string) bool {
	for _, path := range allPkgPaths {
		if path == pkgPath {
			return false
		}
	}
	return true
}

func (p *file) importPkg(this *Package, pkgPath string, testingFile bool) *PkgRef {
	// TODO: canonical pkgPath
	pkgImport, ok := p.importPkgs[pkgPath]
	if !ok {
		pkgImport = &PkgRef{pkg: this, file: p, inTestingFile: testingFile}
		p.importPkgs[pkgPath] = pkgImport
	}
	if !ok || pkgPathNotFound(p.allPkgPaths, pkgPath) {
		p.allPkgPaths = append(p.allPkgPaths, pkgPath)
		p.delayPkgPaths = append(p.delayPkgPaths, pkgPath)
	}
	return pkgImport
}

func (p *file) endImport(this *Package, testingFile bool) {
	pkgPaths := p.delayPkgPaths
	if len(pkgPaths) == 0 {
		return
	}
	if debugImport {
		log.Println("==> LoadPkgs", pkgPaths, testingFile)
	}
	if n := this.loadPkgs(this, p.importPkgs, pkgPaths...); n > 0 {
		log.Panicf("total %d errors\n", n) // TODO: error message
	}
	p.delayPkgPaths = pkgPaths[:0]
}

func (p *file) markUsed(this *Package) {
	if p.removedExprs {
		// travel all ast nodes to mark used
		p.markUsedBy(this, reflect.ValueOf(p.decls))
		return
	}
	// no removed exprs, mark used simplely
	for _, pkgImport := range p.importPkgs {
		if pkgImport.nameRefs != nil {
			pkgImport.isUsed = true
		}
	}
}

var (
	tyAstNode    = reflect.TypeOf((*ast.Node)(nil)).Elem()
	tySelExprPtr = reflect.TypeOf((*ast.SelectorExpr)(nil))
)

func (p *file) markUsedBy(this *Package, val reflect.Value) {
retry:
	switch val.Kind() {
	case reflect.Slice:
		for i, n := 0, val.Len(); i < n; i++ {
			p.markUsedBy(this, val.Index(i))
		}
	case reflect.Ptr:
		if val.IsNil() {
			return
		}
		t := val.Type()
		if t == tySelExprPtr {
			x := val.Interface().(*ast.SelectorExpr).X
			if sym, ok := x.(*ast.Ident); ok {
				name := sym.Name
				for _, at := range p.importPkgs {
					if at.Types.Name() == name { // pkg.Object
						at.markUsed(sym)
					}
				}
			} else {
				p.markUsedBy(this, reflect.ValueOf(x))
			}
		} else if t.Implements(tyAstNode) { // ast.Node
			elem := val.Elem()
			for i, n := 0, elem.NumField(); i < n; i++ {
				p.markUsedBy(this, elem.Field(i))
			}
		}
	case reflect.Interface:
		val = val.Elem()
		goto retry
	}
}

func (p *file) getDecls(this *Package) (decls []ast.Decl) {
	p.markUsed(this)
	n := len(p.allPkgPaths)
	if n == 0 {
		return p.decls
	}
	specs := make([]ast.Spec, 0, n)
	names := this.newAutoNames()
	for _, pkgPath := range p.allPkgPaths {
		pkgImport := p.importPkgs[pkgPath]
		if !pkgImport.isUsed { // unused
			if pkgImport.isForceUsed { // force-used
				specs = append(specs, &ast.ImportSpec{
					Name: underscore, // _
					Path: &ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(pkgPath)},
				})
			}
			continue
		}
		pkgName, renamed := names.RequireName(pkgImport.Types.Name())
		if renamed {
			pkgImport.Types.SetName(pkgName)
			for _, nameRef := range pkgImport.nameRefs {
				nameRef.Name = pkgName
			}
		}
		specs = append(specs, &ast.ImportSpec{
			Name: ident(pkgName),
			Path: &ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(pkgPath)},
		})
	}
	if len(specs) == 0 {
		return p.decls
	}
	decls = make([]ast.Decl, 0, len(p.decls)+1)
	decls = append(decls, &ast.GenDecl{Tok: token.IMPORT, Specs: specs})
	decls = append(decls, p.decls...)
	return
}

func (p *file) big(this *Package, testingFile bool) *PkgRef {
	if p.pkgBig == nil {
		p.pkgBig = p.importPkg(this, "math/big", testingFile)
	}
	return p.pkgBig
}

// ----------------------------------------------------------------------------

// Package type
type Package struct {
	PkgRef
	cb          CodeBuilder
	files       [2]file
	conf        *Config
	prefix      string
	Fset        *token.FileSet
	builtin     *types.Package
	utBigInt    *types.Named
	utBigRat    *types.Named
	utBigFlt    *types.Named
	loadPkgs    LoadPkgsFunc
	autoPrefix  string
	autoIdx     int
	testingFile int
}

// NewPackage creates a new package.
func NewPackage(pkgPath, name string, conf *Config) *Package {
	if conf == nil {
		conf = &Config{}
	}
	prefix := conf.Prefix
	if prefix == "" {
		prefix = defaultNamePrefix
	}
	newBuiltin := conf.NewBuiltin
	if newBuiltin == nil {
		newBuiltin = newBuiltinDefault
	}
	loadPkgs := conf.LoadPkgs
	if loadPkgs == nil {
		loadPkgs = LoadGoPkgs
	}
	files := [2]file{
		{importPkgs: make(map[string]*PkgRef)},
		{importPkgs: make(map[string]*PkgRef)},
	}
	pkg := &Package{
		Fset:       conf.Fset,
		files:      files,
		conf:       conf,
		prefix:     prefix,
		loadPkgs:   loadPkgs,
		autoPrefix: "_auto" + prefix,
	}
	pkg.Types = types.NewPackage(pkgPath, name)
	pkg.builtin = newBuiltin(pkg, prefix, conf)
	pkg.utBigInt = conf.UntypedBigInt
	pkg.utBigRat = conf.UntypedBigRat
	pkg.utBigFlt = conf.UntypedBigFloat
	pkg.cb.init(pkg)
	return pkg
}

// Builtin returns the buitlin package.
func (p *Package) Builtin() *PkgRef {
	return &PkgRef{Types: p.builtin, pkg: p}
}

// CB returns the code builder.
func (p *Package) CB() *CodeBuilder {
	return &p.cb
}

// SetInTestingFile sets inTestingFile or not.
func (p *Package) SetInTestingFile(inTestingFile bool) (old bool) {
	p.testingFile, old = getInTestingFile(inTestingFile), p.InTestingFile()
	return
}

// InTestingFile returns inTestingFile or not.
func (p *Package) InTestingFile() bool {
	return p.testingFile != 0
}

func getInTestingFile(inTestingFile bool) int {
	if inTestingFile {
		return 1
	}
	return 0
}

// HasTestingFile returns true if this package has testing files.
func (p *Package) HasTestingFile() bool {
	return len(p.files[1].decls) != 0
}

// ----------------------------------------------------------------------------
