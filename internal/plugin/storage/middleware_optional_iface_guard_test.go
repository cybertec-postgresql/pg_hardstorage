package storage_test

// A StoragePlugin middleware must forward EVERY optional capability
// interface, not just the ones someone remembered.
//
// The optional-capability pattern is a type assertion: RegionOf asks
// whether sp implements RegionAware, FreeSpaceOf asks whether it
// implements FreeSpaceAware. A wrapper that does not implement one
// FAILS that assertion, and the helper returns its "backend does not
// support this" answer. So the wrapper does not merely fail to
// forward -- it ANSWERS, on behalf of a backend that would have
// answered differently.
//
// That is invisible at the call site. capacity.Preflight recorded
// PreflightUnsupported with the note "backend does not expose
// free-space probe", which is a true sentence about S3 and a false one
// about a throttled fs plugin whose disk-space gate had just been
// silently switched off. Both middlewares forwarded Region and neither
// forwarded FreeSpace -- the pattern was understood and one member of
// it was missed, which is exactly the kind of gap a per-case review
// does not catch and a tree-wide invariant does.
//
// This guard derives BOTH sides from the source rather than from a
// list: the optional interfaces are the "…Aware" interfaces declared
// in package storage, and the middlewares are the types under
// internal/plugin/storage that hold a StoragePlugin field. A new
// optional interface, or a new middleware, is covered the day it lands.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// storageTreeRoot is this file's directory: internal/plugin/storage.
const storageTreeRoot = "."

type parsedFile struct {
	path string
	file *ast.File
}

func parseStorageTree(t *testing.T) []parsedFile {
	t.Helper()
	fset := token.NewFileSet()
	var out []parsedFile
	err := filepath.Walk(storageTreeRoot, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, p, nil, parser.ParseComments)
		if perr != nil {
			return perr
		}
		out = append(out, parsedFile{path: p, file: f})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", storageTreeRoot, err)
	}
	if len(out) == 0 {
		t.Fatal("parsed no files; the guard would pass vacuously")
	}
	return out
}

// optionalInterfaces returns the "…Aware" interfaces declared in
// package storage, mapped to their method names.
func optionalInterfaces(t *testing.T, files []parsedFile) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, pf := range files {
		if pf.file.Name.Name != "storage" {
			continue
		}
		for _, d := range pf.file.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, sp := range gd.Specs {
				ts, ok := sp.(*ast.TypeSpec)
				if !ok || !strings.HasSuffix(ts.Name.Name, "Aware") {
					continue
				}
				it, ok := ts.Type.(*ast.InterfaceType)
				if !ok {
					continue
				}
				var methods []string
				for _, m := range it.Methods.List {
					for _, n := range m.Names {
						methods = append(methods, n.Name)
					}
				}
				if len(methods) > 0 {
					out[ts.Name.Name] = methods
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("found no optional …Aware interfaces in package storage; " +
			"if the pattern was renamed this guard needs updating, not deleting")
	}
	return out
}

// isStoragePluginField reports whether a struct field's type is
// StoragePlugin (or storage.StoragePlugin) -- the marker of a wrapper.
func isStoragePluginField(f *ast.Field) bool {
	switch tv := f.Type.(type) {
	case *ast.Ident:
		return tv.Name == "StoragePlugin"
	case *ast.SelectorExpr:
		return tv.Sel.Name == "StoragePlugin"
	}
	return false
}

// middlewareTypes returns type name -> package path for every struct
// under the storage tree that holds a StoragePlugin field.
func middlewareTypes(t *testing.T, files []parsedFile) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, pf := range files {
		// package storage itself declares the interface; it is not a
		// middleware.
		if pf.file.Name.Name == "storage" {
			continue
		}
		for _, d := range pf.file.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, sp := range gd.Specs {
				ts, ok := sp.(*ast.TypeSpec)
				if !ok {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				for _, fld := range st.Fields.List {
					if isStoragePluginField(fld) {
						out[ts.Name.Name] = filepath.Dir(pf.path)
						break
					}
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("found no middleware types; the guard would pass vacuously")
	}
	return out
}

// declaredMethods returns the set of "TypeName.MethodName" declared
// across the tree.
func declaredMethods(files []parsedFile) map[string]bool {
	out := map[string]bool{}
	for _, pf := range files {
		for _, d := range pf.file.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || len(fd.Recv.List) == 0 {
				continue
			}
			var recv string
			switch rt := fd.Recv.List[0].Type.(type) {
			case *ast.StarExpr:
				if id, ok := rt.X.(*ast.Ident); ok {
					recv = id.Name
				}
			case *ast.Ident:
				recv = rt.Name
			}
			if recv != "" {
				out[recv+"."+fd.Name.Name] = true
			}
		}
	}
	return out
}

func TestMiddlewares_ForwardEveryOptionalCapability(t *testing.T) {
	files := parseStorageTree(t)
	optional := optionalInterfaces(t, files)
	middlewares := middlewareTypes(t, files)
	methods := declaredMethods(files)

	ifaceNames := make([]string, 0, len(optional))
	for k := range optional {
		ifaceNames = append(ifaceNames, k)
	}
	sort.Strings(ifaceNames)
	mwNames := make([]string, 0, len(middlewares))
	for k := range middlewares {
		mwNames = append(mwNames, k)
	}
	sort.Strings(mwNames)
	t.Logf("optional interfaces: %v", ifaceNames)
	t.Logf("middlewares: %v", mwNames)

	for _, mw := range mwNames {
		for _, iface := range ifaceNames {
			for _, meth := range optional[iface] {
				if methods[mw+"."+meth] {
					continue
				}
				t.Errorf("%s (%s) does not implement %s, required by %s.\n\n"+
					"The optional-capability helpers work by type assertion, so a wrapper "+
					"that omits the method does not fall through to the inner plugin -- it "+
					"ANSWERS \"unsupported\" on behalf of a backend that may well support "+
					"it. Forward it the way Region already is:\n\n"+
					"    func (x *%s) %s(...) { return storage.%sOf(...) }",
					mw, middlewares[mw], meth, iface, mw, meth,
					strings.TrimSuffix(iface, "Aware"))
			}
		}
	}
}
