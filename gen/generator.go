// Copyright (c) 2015 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package gen

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"text/template"
)

// Generator tracks code generation state as we generate the output.
type Generator struct {
	importer
	*namespace

	decls []ast.Decl

	listValueLists map[string]struct{}
	setValueLists  map[string]struct{}
	mapItemLists   map[string]struct{}

	listReaders map[string]struct{}
	setReaders  map[string]struct{}
	mapReaders  map[string]struct{}

	// TODO use something to group related decls together
}

// NewGenerator sets up a new generator for Go code.
func NewGenerator() *Generator {
	namespace := newNamespace()
	return &Generator{
		namespace:      namespace,
		importer:       newImporter(namespace),
		listValueLists: make(map[string]struct{}),
		listReaders:    make(map[string]struct{}),
		setValueLists:  make(map[string]struct{}),
		setReaders:     make(map[string]struct{}),
		mapItemLists:   make(map[string]struct{}),
		mapReaders:     make(map[string]struct{}),
	}
}

// TextTemplate renders the given template with the given template context.
func (g *Generator) TextTemplate(s string, data interface{}) (string, error) {
	templateFuncs := template.FuncMap{
		"goCase":          goCase,
		"import":          g.Import,
		"defName":         typeDeclName,
		"newVar":          g.namespace.Child().NewName,
		"toWire":          g.toWire,
		"fromWire":        g.fromWire,
		"typeName":        typeName,
		"typeCode":        g.typeCode,
		"typeReference":   typeReference,
		"isStructType":    isStructType,
		"isReferenceType": isReferenceType,

		"Required": func() fieldRequired { return Required },
		"Optional": func() fieldRequired { return Optional },
		"required": func(b bool) fieldRequired {
			if b {
				return Required
			}
			return Optional
		},
	}

	tmpl, err := template.New("thriftrw").
		Delims("<", ">").Funcs(templateFuncs).Parse(s)
	if err != nil {
		return "", err
	}

	buff := bytes.Buffer{}
	if err := tmpl.Execute(&buff, data); err != nil {
		return "", err
	}

	return buff.String(), nil

}

func (g *Generator) renderTemplate(s string, data interface{}) ([]byte, error) {
	buff := bytes.NewBufferString("package thriftrw\n\n")
	out, err := g.TextTemplate(s, data)
	if err != nil {
		return nil, err
	}
	if _, err := buff.WriteString(out); err != nil {
		return nil, err
	}

	return buff.Bytes(), nil
}

func (g *Generator) recordGenDeclNames(d *ast.GenDecl) error {
	switch d.Tok {
	case token.IMPORT:
		for _, spec := range d.Specs {
			if err := g.AddImportSpec(spec.(*ast.ImportSpec)); err != nil {
				return fmt.Errorf(
					"could not add explicit import %s: %v", spec, err,
				)
			}
		}
	case token.CONST:
		for _, spec := range d.Specs {
			for _, name := range spec.(*ast.ValueSpec).Names {
				if err := g.Reserve(name.Name); err != nil {
					return fmt.Errorf(
						"could not declare constant %q: %v", name.Name, err,
					)
				}
			}
		}
	case token.TYPE:
		for _, spec := range d.Specs {
			name := spec.(*ast.TypeSpec).Name.Name
			if err := g.Reserve(name); err != nil {
				return fmt.Errorf("could not declare type %q: %v", name, err)
			}
		}
	case token.VAR:
		for _, spec := range d.Specs {
			for _, name := range spec.(*ast.ValueSpec).Names {
				if err := g.Reserve(name.Name); err != nil {
					return fmt.Errorf(
						"could not declare var %q: %v", name.Name, err,
					)
				}
			}
		}
	default:
		return fmt.Errorf("unknown declaration: %v", d)
	}
	return nil
}

// DeclareFromTemplate renders a template (in the text/template format) that
// generates Go code and includes all declarations from the template in the code
// generated by the generator.
//
// An error is returned if anything went wrong while generating the template.
//
// For example,
//
// 	g.DeclareFromTemplate(
// 		'type <.Name> int32',
// 		struct{Name string}{Name: "myType"}
// 	)
//
// Will generate,
//
// 	type myType int32
//
// The following functions are available to templates:
//
// goCase(str): Accepts a string and returns it in CamelCase form and the first
// character upper-cased. The string may be ALLCAPS, snake_case, or already
// camelCase.
//
// import(str): Accepts a string and returns the name that should be used in the
// template to refer to that imported module. This helps avoid naming conflicts
// with imports.
//
// 	<$fmt := import "fmt">
// 	<$fmt>.Println("hello world")
//
// newVar(s): Gets a new name that the template can use for a variable without
// worrying about shadowing any globals. Prefers the given string.
//
// 	<$x := newVar "x">
//
// defName(TypeSpec): Takes a TypeSpec representing a **user declared type** and
// returns the name that should be used in the Go code to define that type.
//
// typeReference(TypeSpec, fieldRequired): Takes any TypeSpec and a a value
// indicating whether this reference expects the type to always be present (use
// the "required" function on a boolean, or the "Required" and "Optional"
// functions inside the template to get the corresponding fieldRequired value).
// Returns a string representing a reference to that type, wrapped in a pointer
// if the value was optional.
//
// 	<typeReference $someType Required>
//
// typeCode(TypeSpec): Gets the wire.Type for the given TypeSpec, importing
// the wire module if necessary.
//
// isReferenceType(TypeSpec): Returns true if the given TypeSpec is for a
// reference type.
//
// toWire(TypeSpec, v): Returns an expression of type Value that contains the
// wire representation of the item "v" of type TypeSpec.
//
// fromWire(TypeSpec, v): Returns an expression of type (T, error) where T is
// the type represented by TypeSpec, read from the given Value v.
func (g *Generator) DeclareFromTemplate(s string, data interface{}) error {
	bs, err := g.renderTemplate(s, data)
	if err != nil {
		return err
	}

	f, err := parser.ParseFile(token.NewFileSet(), "thriftrw.go", bs, 0)
	if err != nil {
		return fmt.Errorf("could not parse generated code: %v:\n%s", err, bs)
	}

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil {
				// top-level function
				if err := g.Reserve(d.Name.Name); err != nil {
					return err
				}
			}
		case *ast.GenDecl:
			if err := g.recordGenDeclNames(d); err != nil {
				return err
			}
		default:
			// No special behavior. Move along.
		}
		g.appendDecl(decl)
	}

	return nil
}

// TODO multiple modules

func (g *Generator) Write(w io.Writer, fs *token.FileSet) error {
	// TODO newlines between decls
	// TODO constants first, types next, and functions after that
	// TODO sorting

	decls := make([]ast.Decl, 0, 1+len(g.decls))
	importDecl := g.importDecl()
	if importDecl != nil {
		decls = append(decls, importDecl)
	}
	decls = append(decls, g.decls...)

	file := &ast.File{
		Decls: decls,
		Name:  ast.NewIdent("todo"), // TODO
	}
	return format.Node(w, fs, file)
}

// appendDecl appends a new declaration to the generator.
func (g *Generator) appendDecl(decl ast.Decl) {
	g.decls = append(g.decls, decl)
}
