package generator

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"unicode"

	"github.com/Masterminds/sprig"
	"github.com/pkg/errors"
	"golang.org/x/tools/imports"
)

const (
	skipHolder         = `_`
	parseCommentPrefix = `//`
)

var (
	replacementNames = map[string]string{}
)

// Generator is responsible for generating validation files for the given in a go source file.
type Generator struct {
	Version           string
	Revision          string
	BuildDate         string
	BuiltBy           string
	t                 *template.Template
	knownTemplates    map[string]*template.Template
	userTemplateNames []string
	fileSet           *token.FileSet
	noPrefix          bool
	lowercaseLookup   bool
	caseInsensitive   bool
	marshal           bool
	sql               bool
	flag              bool
	names             bool
	leaveSnakeCase    bool
	prefix            string
	sqlNullInt        bool
	sqlNullStr        bool
	ptr               bool
	mustParse         bool
	forceLower        bool
}

// Enum holds data for a discovered enum in the parsed source
type Enum struct {
	Name   string
	Prefix string
	Type   string
	Values []EnumValue
}

// EnumValue holds the individual data for each enum value within the found enum.
type EnumValue struct {
	RawName      string
	Name         string
	PrefixedName string
	Value        interface{}
	Comment      string
}

// NewGenerator is a constructor method for creating a new Generator with default
// templates loaded.
func NewGenerator() *Generator {
	g := &Generator{
		Version:           "-",
		Revision:          "-",
		BuildDate:         "-",
		BuiltBy:           "-",
		knownTemplates:    make(map[string]*template.Template),
		userTemplateNames: make([]string, 0),
		t:                 template.New("generator"),
		fileSet:           token.NewFileSet(),
		noPrefix:          false,
	}

	funcs := sprig.TxtFuncMap()

	funcs["stringify"] = Stringify
	funcs["mapify"] = Mapify
	funcs["unmapify"] = Unmapify
	funcs["namify"] = Namify
	funcs["offset"] = Offset

	g.t.Funcs(funcs)

	g.addEmbeddedTemplates()

	g.updateTemplates()

	return g
}

// WithNoPrefix is used to change the enum const values generated to not have the enum on them.
func (g *Generator) WithNoPrefix() *Generator {
	g.noPrefix = true
	return g
}

// WithLowercaseVariant is used to change the enum const values generated to not have the enum on them.
func (g *Generator) WithLowercaseVariant() *Generator {
	g.lowercaseLookup = true
	return g
}

// WithLowercaseVariant is used to change the enum const values generated to not have the enum on them.
func (g *Generator) WithCaseInsensitiveParse() *Generator {
	g.lowercaseLookup = true
	g.caseInsensitive = true
	return g
}

// WithMarshal is used to add marshalling to the enum
func (g *Generator) WithMarshal() *Generator {
	g.marshal = true
	return g
}

// WithSQLDriver is used to add marshalling to the enum
func (g *Generator) WithSQLDriver() *Generator {
	g.sql = true
	return g
}

// WithFlag is used to add flag methods to the enum
func (g *Generator) WithFlag() *Generator {
	g.flag = true
	return g
}

// WithNames is used to add Names methods to the enum
func (g *Generator) WithNames() *Generator {
	g.names = true
	return g
}

// WithoutSnakeToCamel is used to add flag methods to the enum
func (g *Generator) WithoutSnakeToCamel() *Generator {
	g.leaveSnakeCase = true
	return g
}

// WithPrefix is used to add a custom prefix to the enum constants
func (g *Generator) WithPrefix(prefix string) *Generator {
	g.prefix = prefix
	return g
}

// WithPtr adds a way to get a pointer value straight from the const value.
func (g *Generator) WithPtr() *Generator {
	g.ptr = true
	return g
}

// WithSQLNullInt is used to add a null int option for SQL interactions.
func (g *Generator) WithSQLNullInt() *Generator {
	g.sqlNullInt = true
	return g
}

// WithSQLNullStr is used to add a null string option for SQL interactions.
func (g *Generator) WithSQLNullStr() *Generator {
	g.sqlNullStr = true
	return g
}

// WithMustParse is used to add a method `MustParse` that will panic on failure.
func (g *Generator) WithMustParse() *Generator {
	g.mustParse = true
	return g
}

// WithForceLower is used to force enums names to lower case while keeping variable names the same.
func (g *Generator) WithForceLower() *Generator {
	g.forceLower = true
	return g
}

// ParseAliases is used to add aliases to replace during name sanitization.
func ParseAliases(aliases []string) error {
	aliasMap := map[string]string{}

	for _, str := range aliases {
		kvps := strings.Split(str, ",")
		for _, kvp := range kvps {
			parts := strings.Split(kvp, ":")
			if len(parts) != 2 {
				return fmt.Errorf("invalid formatted alias entry %q, must be in the format \"key:value\"", kvp)
			}
			aliasMap[parts[0]] = parts[1]
		}
	}

	for k, v := range aliasMap {
		replacementNames[k] = v
	}

	return nil
}

// WithTemplates is used to provide the filenames of additional templates.
func (g *Generator) WithTemplates(filenames ...string) *Generator {
	for _, ut := range template.Must(g.t.ParseFiles(filenames...)).Templates() {
		if _, ok := g.knownTemplates[ut.Name()]; !ok {
			g.userTemplateNames = append(g.userTemplateNames, ut.Name())
		}
	}
	g.updateTemplates()
	sort.Strings(g.userTemplateNames)
	return g
}

// GenerateFromFile is responsible for orchestrating the Code generation.  It results in a byte array
// that can be written to any file desired.  It has already had goimports run on the code before being returned.
func (g *Generator) GenerateFromFile(inputFile string) ([]byte, error) {
	f, err := g.parseFile(inputFile)
	if err != nil {
		return nil, fmt.Errorf("generate: error parsing input file '%s': %s", inputFile, err)
	}
	return g.Generate(f)

}

// Generate does the heavy lifting for the code generation starting from the parsed AST file.
func (g *Generator) Generate(f *ast.File) ([]byte, error) {
	enums := g.inspect(f)
	if len(enums) <= 0 {
		return nil, nil
	}

	pkg := f.Name.Name

	vBuff := bytes.NewBuffer([]byte{})
	err := g.t.ExecuteTemplate(vBuff, "header", map[string]interface{}{
		"package":   pkg,
		"version":   g.Version,
		"revision":  g.Revision,
		"buildDate": g.BuildDate,
		"builtBy":   g.BuiltBy,
	})
	if err != nil {
		return nil, errors.WithMessage(err, "Failed writing header")
	}

	// Make the output more consistent by iterating over sorted keys of map
	var keys []string
	for key := range enums {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, name := range keys {
		ts := enums[name]

		// Parse the enum doc statement
		enum, pErr := g.parseEnum(ts)
		if pErr != nil {
			continue
		}

		data := map[string]interface{}{
			"enum":       enum,
			"name":       name,
			"lowercase":  g.lowercaseLookup,
			"nocase":     g.caseInsensitive,
			"marshal":    g.marshal,
			"sql":        g.sql,
			"flag":       g.flag,
			"names":      g.names,
			"ptr":        g.ptr,
			"sqlnullint": g.sqlNullInt,
			"sqlnullstr": g.sqlNullStr,
			"mustparse":  g.mustParse,
			"forcelower": g.forceLower,
		}

		err = g.t.ExecuteTemplate(vBuff, "enum", data)
		if err != nil {
			return vBuff.Bytes(), errors.WithMessage(err, fmt.Sprintf("Failed writing enum data for enum: %q", name))
		}

		for _, userTemplateName := range g.userTemplateNames {
			err = g.t.ExecuteTemplate(vBuff, userTemplateName, data)
			if err != nil {
				return vBuff.Bytes(), errors.WithMessage(err, fmt.Sprintf("Failed writing enum data for enum: %q, template: %v", name, userTemplateName))
			}
		}
	}

	formatted, err := imports.Process(pkg, vBuff.Bytes(), nil)
	if err != nil {
		err = fmt.Errorf("generate: error formatting code %s\n\n%s", err, vBuff.String())
	}
	return formatted, err
}

// updateTemplates will update the lookup map for validation checks that are
// allowed within the template engine.
func (g *Generator) updateTemplates() {
	for _, template := range g.t.Templates() {
		g.knownTemplates[template.Name()] = template
	}
}

// parseFile simply calls the go/parser ParseFile function with an empty token.FileSet
func (g *Generator) parseFile(fileName string) (*ast.File, error) {
	// Parse the file given in arguments
	return parser.ParseFile(g.fileSet, fileName, nil, parser.ParseComments)
}

// parseEnum looks for the ENUM(x,y,z) formatted documentation from the type definition
func (g *Generator) parseEnum(ts *ast.TypeSpec) (*Enum, error) {

	if ts.Doc == nil {
		return nil, errors.New("No Doc on Enum")
	}

	enum := &Enum{}

	enum.Name = ts.Name.Name
	enum.Type = fmt.Sprintf("%s", ts.Type)
	if !g.noPrefix {
		enum.Prefix = ts.Name.Name
	}
	if g.prefix != "" {
		enum.Prefix = g.prefix + enum.Prefix
	}

	enumDecl := getEnumDeclFromComments(ts.Doc.List)

	values := strings.Split(strings.TrimSuffix(strings.TrimPrefix(enumDecl, `ENUM(`), `)`), `,`)
	var (
		data     interface{}
		unsigned bool
	)
	if strings.HasPrefix(enum.Type, "u") {
		data = uint64(0)
		unsigned = true
	} else {
		data = int64(0)
	}
	for _, value := range values {
		var comment string

		// Trim and store comments
		if strings.Contains(value, parseCommentPrefix) {
			commentStartIndex := strings.Index(value, parseCommentPrefix)
			comment = value[commentStartIndex+len(parseCommentPrefix):]
			comment = strings.TrimSpace(unescapeComment(comment))
			// value without comment
			value = value[:commentStartIndex]
		}

		// Make sure to leave out any empty parts
		if value != "" {
			if strings.Contains(value, `=`) {
				// Get the value specified and set the data to that value.
				equalIndex := strings.Index(value, `=`)
				dataVal := strings.TrimSpace(value[equalIndex+1:])
				if dataVal != "" {
					if unsigned {
						newData, err := strconv.ParseUint(dataVal, 10, 64)
						if err != nil {
							err = errors.Wrapf(err, "failed parsing the data part of enum value '%s'", value)
							fmt.Println(err)
							return nil, err
						}
						data = newData
					} else {
						newData, err := strconv.ParseInt(dataVal, 10, 64)
						if err != nil {
							err = errors.Wrapf(err, "failed parsing the data part of enum value '%s'", value)
							fmt.Println(err)
							return nil, err
						}
						data = newData
					}
					value = value[:equalIndex]
				} else {
					value = strings.TrimSuffix(value, `=`)
					fmt.Printf("Ignoring enum with '=' but no value after: %s\n", value)
				}
			}
			rawName := strings.TrimSpace(value)
			name := strings.Title(rawName)
			prefixedName := name
			if name != skipHolder {
				prefixedName = enum.Prefix + name
				prefixedName = sanitizeValue(prefixedName)
				if !g.leaveSnakeCase {
					prefixedName = snakeToCamelCase(prefixedName)
				}
			}

			ev := EnumValue{Name: name, RawName: rawName, PrefixedName: prefixedName, Value: data, Comment: comment}
			enum.Values = append(enum.Values, ev)
			data = increment(data)
		}
	}

	// fmt.Printf("###\nENUM: %+v\n###\n", enum)

	return enum, nil
}

func increment(d interface{}) interface{} {
	switch v := d.(type) {
	case uint64:
		return v + 1
	case int64:
		return v + 1
	}
	return d
}

func unescapeComment(comment string) string {
	val, err := url.QueryUnescape(comment)
	if err != nil {
		return comment
	}
	return val
}

// sanitizeValue will ensure the value name generated adheres to golang's
// identifier syntax as described here: https://golang.org/ref/spec#Identifiers
// identifier = letter { letter | unicode_digit }
// where letter can be unicode_letter or '_'
func sanitizeValue(value string) string {
	// Keep skip value holders
	if value == skipHolder {
		return skipHolder
	}

	replacedValue := value
	for k, v := range replacementNames {
		replacedValue = strings.ReplaceAll(replacedValue, k, v)
	}

	nameBuilder := strings.Builder{}
	nameBuilder.Grow(len(replacedValue))

	for i, r := range replacedValue {
		// If the start character is not a unicode letter (this check includes the case of '_')
		// then we need to add an exported prefix, so tack on a 'X' at the beginning
		if i == 0 && !unicode.IsLetter(r) {
			nameBuilder.WriteRune('X')
		}

		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' {
			nameBuilder.WriteRune(r)
		}
	}

	return nameBuilder.String()
}

func snakeToCamelCase(value string) string {
	parts := strings.Split(value, "_")
	for i, part := range parts {
		parts[i] = strings.Title(part)
	}
	value = strings.Join(parts, "")

	return value
}

// getEnumDeclFromComments parses the array of comment strings and creates a single Enum Declaration statement
// that is easier to deal with for the remainder of parsing.  It turns multi line declarations and makes a single
// string declaration.
func getEnumDeclFromComments(comments []*ast.Comment) string {
	parts := []string{}
	store := false

	lines := []string{}

	for _, comment := range comments {
		lines = append(lines, breakCommentIntoLines(comment)...)
	}

	enumParamLevel := 0
	// Go over all the lines in this comment block
	for _, line := range lines {
		if store {
			paramLevel, trimmed := parseLinePart(line)
			if trimmed != "" {
				parts = append(parts, trimmed)
			}
			enumParamLevel += paramLevel
			if enumParamLevel == 0 {
				// End ENUM Declaration
				if trimmed != "" {
					end := strings.Index(trimmed, ")")
					if end >= 0 {
						parts[len(parts)-1] = trimmed[:end]
					}
				}
				break
			}
		}
		if strings.Contains(line, `ENUM(`) {
			enumParamLevel = 1
			startIndex := strings.Index(line, `ENUM(`)
			if startIndex >= 0 {
				line = line[startIndex+len(`ENUM(`):]
			}
			paramLevel, trimmed := parseLinePart(line)
			if trimmed != "" {
				parts = append(parts, trimmed)
			}
			enumParamLevel += paramLevel

			// Start ENUM Declaration
			if enumParamLevel > 0 {
				// Store other lines
				store = true
			}
		}
	}

	if enumParamLevel > 0 {
		fmt.Println("ENUM Parse error, there is a dangling '(' in your comment.")
	}
	joined := fmt.Sprintf("ENUM(%s)", strings.Join(parts, `,`))
	return joined
}

func parseLinePart(line string) (paramLevel int, trimmed string) {
	trimmed = line
	comment := ""
	if idx := strings.Index(line, parseCommentPrefix); idx >= 0 {
		trimmed = line[:idx]
		comment = "//" + url.QueryEscape(strings.TrimSpace(line[idx+2:]))
	}
	trimmed = trimAllTheThings(trimmed)
	trimmed += comment
	opens := strings.Count(line, `(`)
	closes := strings.Count(line, `)`)
	if opens > 0 {
		paramLevel += opens
	}
	if closes > 0 {
		paramLevel -= closes
	}
	return
}

// breakCommentIntoLines takes the comment and since single line comments are already broken into lines
// we break multiline comments into separate lines for processing.
func breakCommentIntoLines(comment *ast.Comment) []string {
	lines := []string{}
	text := comment.Text
	if strings.HasPrefix(text, `/*`) {
		// deal with multi line comment
		multiline := strings.TrimSuffix(strings.TrimPrefix(text, `/*`), `*/`)
		lines = append(lines, strings.Split(multiline, "\n")...)
	} else {
		lines = append(lines, strings.TrimPrefix(text, `//`))
	}
	return lines
}

// trimAllTheThings takes off all the cruft of a line that we don't need.
func trimAllTheThings(thing string) string {
	preTrimmed := strings.TrimSuffix(strings.TrimSpace(thing), `,`)
	end := strings.Index(preTrimmed, `)`)

	if end < 0 {
		end = len(preTrimmed)
	}

	return strings.TrimSpace(preTrimmed[:end])
}

// inspect will walk the ast and fill a map of names and their struct information
// for use in the generation template.
func (g *Generator) inspect(f ast.Node) map[string]*ast.TypeSpec {
	enums := make(map[string]*ast.TypeSpec)
	// Inspect the AST and find all structs.
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.GenDecl:
			copyGenDeclCommentsToSpecs(x)
		case *ast.Ident:
			if x.Obj != nil {
				// fmt.Printf("Node: %#v\n", x.Obj)
				// Make sure it's a Type Identifier
				if x.Obj.Kind == ast.Typ {
					// Make sure it's a spec (Type Identifiers can be throughout the code)
					if ts, ok := x.Obj.Decl.(*ast.TypeSpec); ok {
						// fmt.Printf("Type: %+v\n", ts)
						isEnum := isTypeSpecEnum(ts)
						// Only store documented enums
						if isEnum {
							// fmt.Printf("EnumType: %T\n", ts.Type)
							enums[x.Name] = ts
						}
					}
				}
			}
		}
		// Return true to continue through the tree
		return true
	})

	return enums
}

// copyDocsToSpecs will take the GenDecl level documents and copy them
// to the children Type and Value specs.  I think this is actually working
// around a bug in the AST, but it works for now.
func copyGenDeclCommentsToSpecs(x *ast.GenDecl) {
	// Copy the doc spec to the type or value spec
	// cause they missed this... whoops
	if x.Doc != nil {
		for _, spec := range x.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				if s.Doc == nil {
					s.Doc = x.Doc
				}
			case *ast.ValueSpec:
				if s.Doc == nil {
					s.Doc = x.Doc
				}
			}
		}
	}

}

// isTypeSpecEnum checks the comments on the type spec to determine if there is an enum
// declaration for the type.
func isTypeSpecEnum(ts *ast.TypeSpec) bool {
	isEnum := false
	if ts.Doc != nil {
		for _, comment := range ts.Doc.List {
			if strings.Contains(comment.Text, `ENUM(`) {
				isEnum = true
			}
		}
	}

	return isEnum
}
