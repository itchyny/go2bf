package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
)

// AnalysisResult holds the result of semantic analysis.
type AnalysisResult struct {
	Funcs        map[string]*FuncInfo
	Structs      map[string]*StructDef
	ByteConsts   map[string]byte   // compile-time byte constants
	IntConsts    map[string]uint64 // compile-time multi-byte integer constants (uint16/uint32/uint64)
	IntConstSize map[string]int    // constant name -> integer size (2, 4, or 8)
	StringConsts map[string]string // compile-time string constants
	fset         *token.FileSet
}

// FuncInfo holds analysis results for a function.
type FuncInfo struct {
	Name        string
	Params      []string        // parameter names in order
	ParamTypes  []ParamInfo     // parameter names with type info
	Returns     int             // total return cell count across all return values
	ReturnSizes []int           // per-return-value cell counts (nil for all-byte)
	ReturnNames []string        // named return variable names (nil if unnamed)
	ReturnType  ReturnInfo      // composite return type info
	Body        *ast.BlockStmt  // function body AST
	Calls       map[string]bool // names of user-defined functions called
	IsRecursive bool            // true if function is (mutually) recursive
	IsTailRec   bool            // true if all recursive calls are tail calls
}

// ParamInfo holds a function parameter's name and optional composite type.
type ParamInfo struct {
	Name             string
	ArraySize        int        // >0 if the parameter is an array (total cells)
	ArrayCount       int        // >0 for arrays: number of elements
	ArrayElemSize    int        // >0 for arrays: cells per element
	ArrayElemType    string     // non-empty for arrays of structs
	ArrayElemIntSize int        // >1 for arrays of multi-byte integers
	ArrayElemSlice   bool       // true for arrays of slices ([N]string, [N][]byte)
	StructType       string     // non-empty if the parameter is a struct type
	IsSlice          bool       // true if []byte or []StructType
	IsPointer        bool       // true if *byte, *[N]byte, *uintN, or *StructType
	IntSize          int        // >1 for multi-byte integers (2, 4, or 8)
	PtrArrayInfo     *ParamInfo // non-nil for *[N]byte -- inner array info
	PtrStructType    string     // non-empty for *StructType
	PtrIntSize       int        // >1 for *uintN -- pointed-to integer width
}

// ReturnInfo describes a function's return type.
type ReturnInfo struct {
	ArraySize        int    // >0 if returning a [N]byte or *[N]byte
	StructType       string // non-empty if returning a struct
	IsSlice          bool   // true if returning a slice
	IsPointer        bool   // true if returning a pointer (*[N]byte)
	SliceElemSize    int    // cells per slice element (1 for byte)
	SliceElemType    string // struct type name for slice elements
	SliceElemIntSize int    // >1 for slices of multi-byte integers
	SliceElemSlice   bool   // true for slice of slices ([]string, [][]byte)
	IntSize          int    // >1 for multi-byte integer returns (2, 4, or 8)
}

// StructDef holds a struct type definition.
type StructDef struct {
	Name                  string
	Fields                []string          // field names in order
	Offsets               map[string]int    // field name -> offset
	FieldTypes            map[string]string // field name -> struct type name (empty for byte)
	FieldArraySizes       map[string]int    // field name -> array size (0 for non-array)
	FieldInnerSizes       map[string]int    // field name -> inner element size for nested array fields
	FieldArrayElemIntSize map[string]int    // field name -> element width for multi-byte int array fields
	FieldArrayElemType    map[string]string // field name -> struct type name of array elements (for [N]Item)
	FieldIntSizes         map[string]int    // field name -> integer size (2, 4, or 8)
	FieldStrings          map[string]bool   // field name -> true if string (3-cell []byte header)
	Size                  int               // total number of cells
}

// arrayFieldInfo returns (totalSize, innerElemSize) for an array type expression.
// totalSize is total cells; innerElemSize is the inner array's element size for
// nested arrays (0 if flat). For [N]byte: (N, 0). For [N][M]byte: (N*M, M).
// For [N]uint16: (N*2, 0). For [N]uint32: (N*4, 0). For [N]uint64: (N*8, 0).
func arrayFieldInfo(expr ast.Expr) (int, int) {
	at, ok := expr.(*ast.ArrayType)
	if !ok || at.Len == nil {
		return 0, 0
	}
	n := arrayTypeSize(expr)
	if n <= 0 {
		return 0, 0
	}
	if innerAt, ok := at.Elt.(*ast.ArrayType); ok && innerAt.Len != nil {
		innerSize, _ := arrayFieldInfo(at.Elt)
		if innerSize > 0 {
			return n * innerSize, innerSize
		}
	}
	if id, ok := at.Elt.(*ast.Ident); ok {
		if w := intIdentSize(id.Name); w > 0 {
			return n * w, 0
		}
	}
	return n, 0
}

// intIdentSize returns the byte size for integer type names:
// "uint16" -> 2, "uint32" -> 4, "uint64" -> 8, others -> 0.
func intIdentSize(name string) int {
	switch name {
	case "uint16":
		return 2
	case "uint32":
		return 4
	case "uint64":
		return 8
	}
	return 0
}

// intTypeSize returns the byte size for a uintN type expression
// (2, 4, or 8), or 0 for any other expression.
func intTypeSize(expr ast.Expr) int {
	if id, ok := expr.(*ast.Ident); ok {
		return intIdentSize(id.Name)
	}
	return 0
}

// classifyIntConst picks the cell size (1 for byte, 2/4/8 for multi-byte) of
// an integer constant, given its declared type size (intSize == 0 for untyped)
// and value. Returns an error if val is outside the resolved type's range.
// Untyped constants are promoted to the smallest size that fits the value.
func classifyIntConst(name string, val, intSize int) (int, error) {
	if intSize == 0 {
		switch {
		case val > math.MaxUint32:
			intSize = 8
		case val > math.MaxUint16:
			intSize = 4
		case val > math.MaxUint8:
			intSize = 2
		default:
			intSize = 1
		}
	}
	maxVal := uint64(1)<<(intSize*8) - 1
	if val < 0 || uint64(val) > maxVal {
		typeName := "byte"
		if intSize >= 2 {
			typeName = fmt.Sprintf("uint%d", intSize*8)
		}
		return 0, fmt.Errorf("const %s: value %d out of %s range (0-%d)", name, val, typeName, maxVal)
	}
	return intSize, nil
}

// Analyze performs semantic analysis on the ASTs.
func Analyze(files []*ast.File, fset *token.FileSet) (*AnalysisResult, error) {
	result := &AnalysisResult{
		Funcs:        make(map[string]*FuncInfo),
		Structs:      make(map[string]*StructDef),
		ByteConsts:   make(map[string]byte),
		IntConsts:    make(map[string]uint64),
		IntConstSize: make(map[string]int),
		StringConsts: make(map[string]string),
		fset:         fset,
	}

	for _, file := range files {
		if file.Name.Name != "main" {
			return nil, fmt.Errorf("%s: expected package main, got package %s",
				fset.Position(file.Pos()).Filename, file.Name.Name)
		}
		if len(file.Imports) > 0 {
			pos := fset.Position(file.Imports[0].Pos())
			return nil, fmt.Errorf("%s: imports are not supported", pos)
		}
		for _, decl := range file.Decls {
			// Parse const declarations (supports iota, char literals, const blocks).
			if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.CONST {
				iota := 0
				var lastExprs []ast.Expr // repeat previous expressions for iota
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					if len(vs.Values) > 0 {
						lastExprs = vs.Values
					}
					for i, name := range vs.Names {
						if i < len(lastExprs) {
							// String-typed constants (literal, ident reference, or concat).
							if s, ok := evalStringConstExpr(lastExprs[i], result.StringConsts); ok {
								result.StringConsts[name.Name] = s
								continue
							}
							val, err := evalConstExpr(lastExprs[i], iota, result.ByteConsts)
							if err != nil {
								return nil, fmt.Errorf("const %s: %w", name.Name, err)
							}
							size, err := classifyIntConst(name.Name, val, intTypeSize(vs.Type))
							if err != nil {
								return nil, err
							}
							if size > 1 {
								result.IntConsts[name.Name] = uint64(val) // #nosec G115
								result.IntConstSize[name.Name] = size
							} else {
								result.ByteConsts[name.Name] = byte(val) // #nosec G115
							}
						}
					}
					iota++
				}
				continue
			}

			// Parse struct type definitions.
			if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.TYPE {
				for _, spec := range gd.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok {
						continue
					}
					def := &StructDef{
						Name:                  ts.Name.Name,
						Offsets:               make(map[string]int),
						FieldTypes:            make(map[string]string),
						FieldArraySizes:       make(map[string]int),
						FieldInnerSizes:       make(map[string]int),
						FieldArrayElemIntSize: make(map[string]int),
						FieldArrayElemType:    make(map[string]string),
						FieldIntSizes:         make(map[string]int),
						FieldStrings:          make(map[string]bool),
					}
					offset := 0
					for _, field := range st.Fields.List {
						fieldSize := 1 // default: byte
						fieldType := ""
						fieldArraySize := 0
						fieldArrayElemIntSize := 0
						fieldArrayElemType := ""
						fieldIsString := false
						if id, ok := field.Type.(*ast.Ident); ok {
							if nested, ok := result.Structs[id.Name]; ok {
								fieldSize = nested.Size
								fieldType = id.Name
							} else if n := intIdentSize(id.Name); n > 0 {
								fieldSize = n
							} else if id.Name == "string" {
								fieldSize = 3 // ptr, len, cap
								fieldIsString = true
							}
						} else if at, ok := field.Type.(*ast.ArrayType); ok && at.Len != nil {
							arrSize, ies := arrayFieldInfo(field.Type)
							if arrSize > 0 {
								fieldSize = arrSize
								fieldArraySize = arrSize
								if ies > 0 {
									for _, name := range field.Names {
										def.FieldInnerSizes[name.Name] = ies
									}
								}
								// Detect [N]uintN element type for multi-byte int arrays.
								if eltID, ok := at.Elt.(*ast.Ident); ok {
									if n := intIdentSize(eltID.Name); n > 0 {
										fieldArrayElemIntSize = n
									} else if _, ok := result.Structs[eltID.Name]; ok {
										fieldArrayElemType = eltID.Name
									}
								}
							}
						}
						for _, name := range field.Names {
							def.Fields = append(def.Fields, name.Name)
							def.Offsets[name.Name] = offset
							if fieldType != "" {
								def.FieldTypes[name.Name] = fieldType
							}
							if fieldArraySize > 0 {
								def.FieldArraySizes[name.Name] = fieldArraySize
							}
							if fieldArrayElemIntSize > 0 {
								def.FieldArrayElemIntSize[name.Name] = fieldArrayElemIntSize
							}
							if fieldArrayElemType != "" {
								def.FieldArrayElemType[name.Name] = fieldArrayElemType
							}
							if fieldIsString {
								def.FieldStrings[name.Name] = true
							} else if fieldSize >= 2 && fieldType == "" && fieldArraySize == 0 {
								def.FieldIntSizes[name.Name] = fieldSize
							}
							offset += fieldSize
						}
					}
					def.Size = offset
					if _, exists := result.Structs[def.Name]; exists {
						return nil, fmt.Errorf("duplicate type: %s", def.Name)
					}
					result.Structs[def.Name] = def
				}
				continue
			}

			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			funcName := fn.Name.Name
			// Method receiver: func (p Point) name() -> stored as "Point.name"
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				recvField := fn.Recv.List[0]
				if recvType, ok := recvField.Type.(*ast.Ident); ok {
					funcName = recvType.Name + "." + fn.Name.Name
				}
			}

			if _, exists := result.Funcs[funcName]; exists {
				return nil, fmt.Errorf("duplicate function: %s", funcName)
			}
			info := &FuncInfo{
				Name:  funcName,
				Body:  fn.Body,
				Calls: make(map[string]bool),
			}

			// Prepend receiver as first parameter for methods.
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				recvField := fn.Recv.List[0]
				var structType string
				if recvType, ok := recvField.Type.(*ast.Ident); ok {
					if _, ok := result.Structs[recvType.Name]; ok {
						structType = recvType.Name
					}
				}
				for _, name := range recvField.Names {
					info.Params = append(info.Params, name.Name)
					info.ParamTypes = append(info.ParamTypes, ParamInfo{
						Name:       name.Name,
						StructType: structType,
					})
				}
			}

			// Extract parameter names and types.
			if fn.Type.Params != nil {
				for _, field := range fn.Type.Params.List {
					var pi ParamInfo
					if at, ok := field.Type.(*ast.ArrayType); ok {
						if at.Len == nil {
							// Slice parameter: []byte or []Point.
							pi.IsSlice = true
						}
						count := arrayTypeSize(field.Type)
						if count > 0 {
							elemSize := 1
							elemType := ""
							elemIntSize := 0
							elemSlice := false
							if id, ok := at.Elt.(*ast.Ident); ok {
								if def, ok := result.Structs[id.Name]; ok {
									elemSize = def.Size
									elemType = id.Name
								} else if n := intIdentSize(id.Name); n > 0 {
									elemSize = n
									elemIntSize = n
								} else if id.Name == "string" {
									elemSize = 3
									elemSlice = true
								}
							} else if eltAt, ok := at.Elt.(*ast.ArrayType); ok && eltAt.Len == nil {
								elemSize = 3
								elemSlice = true
							} else if innerSize := arrayTypeSize(at.Elt); innerSize > 0 {
								elemSize = innerSize
							}
							pi.ArraySize = count * elemSize
							pi.ArrayCount = count
							pi.ArrayElemSize = elemSize
							pi.ArrayElemType = elemType
							pi.ArrayElemIntSize = elemIntSize
							pi.ArrayElemSlice = elemSlice
						}
					} else if id, ok := field.Type.(*ast.Ident); ok {
						if _, ok := result.Structs[id.Name]; ok {
							pi.StructType = id.Name
						} else if n := intIdentSize(id.Name); n > 0 {
							pi.IntSize = n
						} else if id.Name == "string" {
							pi.IsSlice = true
							pi.ArrayElemSize = 1
						}
					} else if star, ok := field.Type.(*ast.StarExpr); ok {
						pi.IsPointer = true
						if id, ok := star.X.(*ast.Ident); ok {
							if n := intIdentSize(id.Name); n > 0 {
								pi.PtrIntSize = n
							}
						}
						if at, ok := star.X.(*ast.ArrayType); ok {
							count := arrayTypeSizePart(at.Len, result.ByteConsts)
							if count > 0 {
								elemSize := 1
								elemType := ""
								if eid, ok := at.Elt.(*ast.Ident); ok {
									if def, ok := result.Structs[eid.Name]; ok {
										elemSize = def.Size
										elemType = eid.Name
									}
								}
								pi.PtrArrayInfo = &ParamInfo{
									ArraySize:     count * elemSize,
									ArrayCount:    count,
									ArrayElemSize: elemSize,
									ArrayElemType: elemType,
								}
							}
						} else if id, ok := star.X.(*ast.Ident); ok {
							if _, ok := result.Structs[id.Name]; ok {
								pi.PtrStructType = id.Name
							}
						}
					}
					for _, name := range field.Names {
						pi.Name = name.Name
						info.Params = append(info.Params, name.Name)
						info.ParamTypes = append(info.ParamTypes, pi)
					}
				}
			}

			// Count return values and detect composite return types.
			if fn.Type.Results != nil {
				for _, field := range fn.Type.Results.List {
					retSize := 1
					if id, ok := field.Type.(*ast.Ident); ok {
						if n := intIdentSize(id.Name); n > 0 {
							retSize = n
						} else if id.Name == "string" {
							retSize = 3
						}
					}
					if len(field.Names) == 0 {
						info.Returns += retSize
						info.ReturnSizes = append(info.ReturnSizes, retSize)
					} else {
						for _, name := range field.Names {
							info.ReturnNames = append(info.ReturnNames, name.Name)
							info.ReturnSizes = append(info.ReturnSizes, retSize)
						}
						info.Returns += len(field.Names) * retSize
					}
				}
				// Detect array/struct return type (single return value only).
				if len(info.ReturnSizes) == 1 && len(fn.Type.Results.List) == 1 {
					retType := fn.Type.Results.List[0].Type
					if size := arrayTypeSize(retType); size > 0 {
						info.ReturnType.ArraySize = size
					} else if id, ok := retType.(*ast.Ident); ok {
						if _, ok := result.Structs[id.Name]; ok {
							info.ReturnType.StructType = id.Name
						} else if n := intIdentSize(id.Name); n > 0 {
							info.ReturnType.IntSize = n
						} else if id.Name == "string" {
							info.ReturnType.IsSlice = true
							info.ReturnType.SliceElemSize = 1
							info.Returns = 3
						}
					} else if star, ok := retType.(*ast.StarExpr); ok {
						if size := arrayTypeSize(star.X); size > 0 {
							info.ReturnType.ArraySize = size
							info.ReturnType.IsPointer = true
						} else if id, ok := star.X.(*ast.Ident); ok {
							if _, ok := result.Structs[id.Name]; ok {
								info.ReturnType.StructType = id.Name
								info.ReturnType.IsPointer = true
							}
						}
					} else if isSliceType(retType) {
						info.ReturnType.IsSlice = true
						info.Returns = 3 // ptr, len, cap
						at := retType.(*ast.ArrayType)
						if id, ok := at.Elt.(*ast.Ident); ok {
							if def, ok := result.Structs[id.Name]; ok {
								info.ReturnType.SliceElemSize = def.Size
								info.ReturnType.SliceElemType = id.Name
							} else if n := intIdentSize(id.Name); n > 0 {
								info.ReturnType.SliceElemSize = n
								info.ReturnType.SliceElemIntSize = n
							} else if id.Name == "string" {
								info.ReturnType.SliceElemSize = 3
								info.ReturnType.SliceElemSlice = true
							}
						}
						if eltAt, ok := at.Elt.(*ast.ArrayType); ok && eltAt.Len == nil {
							// `[][]byte` or any other `[][]T` slice element
							info.ReturnType.SliceElemSize = 3
							info.ReturnType.SliceElemSlice = true
						}
						if size := arrayTypeSize(at.Elt); size > 0 {
							info.ReturnType.SliceElemSize = size
						}
					}
				}
			}

			result.Funcs[funcName] = info
		}
	}

	if _, ok := result.Funcs["main"]; !ok {
		return nil, fmt.Errorf("no main function found")
	}

	// Build call graph: find calls to user-defined functions.
	for _, info := range result.Funcs {
		ast.Inspect(info.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			if _, isUserFunc := result.Funcs[ident.Name]; isUserFunc {
				info.Calls[ident.Name] = true
			}
			return true
		})
	}

	// Detect recursion and tail-call recursion.
	if err := detectRecursion(result); err != nil {
		return nil, err
	}

	return result, nil
}

// evalStringConstExpr folds a string-typed constant expression at compile
// time. Handles string literals, references to known string constants,
// and concatenation chains thereof. Returns (value, true) if foldable.
func evalStringConstExpr(expr ast.Expr, stringConsts map[string]string) (string, bool) {
	if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		s, err := strconv.Unquote(lit.Value)
		return s, err == nil
	}
	if id, ok := expr.(*ast.Ident); ok {
		s, ok := stringConsts[id.Name]
		return s, ok
	}
	if bin, ok := expr.(*ast.BinaryExpr); ok && bin.Op == token.ADD {
		l, ok := evalStringConstExpr(bin.X, stringConsts)
		if !ok {
			return "", false
		}
		r, ok := evalStringConstExpr(bin.Y, stringConsts)
		if !ok {
			return "", false
		}
		return l + r, true
	}
	return "", false
}

// evalConstExpr evaluates a constant expression to an integer value.
func evalConstExpr(expr ast.Expr, iota int, consts map[string]byte) (int, error) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		switch e.Kind {
		case token.INT:
			val, err := strconv.ParseInt(e.Value, 0, 64)
			if err != nil {
				return 0, err
			}
			return int(val), nil
		case token.CHAR:
			ch, _, _, err := strconv.UnquoteChar(e.Value[1:len(e.Value)-1], '\'')
			if err != nil {
				return 0, err
			}
			return int(ch), nil
		}
	case *ast.Ident:
		if e.Name == "iota" {
			return iota, nil
		}
		if val, ok := consts[e.Name]; ok {
			return int(val), nil
		}
	case *ast.BinaryExpr:
		left, err := evalConstExpr(e.X, iota, consts)
		if err != nil {
			return 0, err
		}
		right, err := evalConstExpr(e.Y, iota, consts)
		if err != nil {
			return 0, err
		}
		switch e.Op {
		case token.ADD:
			return left + right, nil
		case token.SUB:
			return left - right, nil
		case token.MUL:
			return left * right, nil
		case token.QUO:
			if right == 0 {
				return 0, fmt.Errorf("division by zero in constant expression")
			}
			return left / right, nil
		case token.REM:
			if right == 0 {
				return 0, fmt.Errorf("modulo by zero in constant expression")
			}
			return left % right, nil
		case token.AND:
			return left & right, nil
		case token.OR:
			return left | right, nil
		case token.XOR:
			return left ^ right, nil
		case token.AND_NOT:
			return left &^ right, nil
		case token.SHL:
			return left << right, nil
		case token.SHR:
			return left >> right, nil
		}
	case *ast.CallExpr:
		// Handle byte() type conversion.
		if id, ok := e.Fun.(*ast.Ident); ok && id.Name == "byte" && len(e.Args) == 1 {
			return evalConstExpr(e.Args[0], iota, consts)
		}
	case *ast.UnaryExpr:
		val, err := evalConstExpr(e.X, iota, consts)
		if err != nil {
			return 0, err
		}
		switch e.Op {
		case token.SUB:
			return -val, nil
		case token.XOR:
			return ^val & 0xFF, nil
		}
	case *ast.ParenExpr:
		return evalConstExpr(e.X, iota, consts)
	}
	return 0, fmt.Errorf("unsupported constant expression")
}

// detectRecursion marks functions that are part of call graph cycles.
func detectRecursion(result *AnalysisResult) error {
	for _, name := range slices.Sorted(maps.Keys(result.Funcs)) {
		info := result.Funcs[name]
		if canReach(result, name, name, make(map[string]bool)) {
			info.IsRecursive = true
			info.IsTailRec = isTailRecursive(info)
			// Check for mutual recursion: if any callee can reach
			// this function, it's a mutual recursion cycle.
			for _, callee := range slices.Sorted(maps.Keys(info.Calls)) {
				if callee != name {
					if path := findCyclePath(result, callee, name); path != nil {
						cycle := name + " -> " + strings.Join(path, " -> ")
						return fmt.Errorf("mutual recursion is not supported: %s", cycle)
					}
				}
			}
		}
	}
	return nil
}

// findCyclePath returns the path from 'from' to 'target' through the call graph,
// or nil if no path exists.
func findCyclePath(result *AnalysisResult, from, target string) []string {
	var dfs func(cur string, visited map[string]bool) []string
	dfs = func(cur string, visited map[string]bool) []string {
		if cur == target {
			return []string{cur}
		}
		info, ok := result.Funcs[cur]
		if !ok {
			return nil
		}
		for callee := range info.Calls {
			if !visited[callee] {
				visited[callee] = true
				if path := dfs(callee, visited); path != nil {
					return append([]string{cur}, path...)
				}
			}
		}
		return nil
	}
	visited := map[string]bool{from: true}
	return dfs(from, visited)
}

// canReach checks if 'from' can reach 'target' through the call graph.
func canReach(result *AnalysisResult, from, target string, visited map[string]bool) bool {
	info, ok := result.Funcs[from]
	if !ok {
		return false
	}
	for callee := range info.Calls {
		if callee == target {
			return true
		}
		if !visited[callee] {
			visited[callee] = true
			if canReach(result, callee, target, visited) {
				return true
			}
		}
	}
	return false
}

// isTailRecursive checks if all recursive self-calls are in tail position.
// Functions with defer cannot use tail-call optimization because the loop
// rewrite loses per-call defer semantics.
func isTailRecursive(info *FuncInfo) bool {
	if info.Returns == 0 || hasDefer(info.Body) {
		return false
	}
	hasSelfCall := false
	allTail := true
	inspectTailCalls(info.Body.List, info.Name, &hasSelfCall, &allTail)
	return hasSelfCall && allTail
}

// hasDefer reports whether a block contains any defer statements.
func hasDefer(block *ast.BlockStmt) bool {
	found := false
	ast.Inspect(block, func(n ast.Node) bool {
		if _, ok := n.(*ast.DeferStmt); ok {
			found = true
		}
		return !found
	})
	return found
}

// inspectTailCalls checks whether all self-recursive calls in stmts are in tail position.
func inspectTailCalls(stmts []ast.Stmt, funcName string, hasSelfCall, allTail *bool) {
	for _, stmt := range stmts {
		switch s := stmt.(type) {
		case *ast.ReturnStmt:
			// Check if the return expression is a self-call.
			if len(s.Results) == 1 {
				if call, ok := s.Results[0].(*ast.CallExpr); ok {
					if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == funcName {
						*hasSelfCall = true
						// Check arguments for nested self-calls (not tail).
						for _, arg := range call.Args {
							checkNonTailCallsExpr(arg, funcName, hasSelfCall, allTail)
						}
						continue
					}
				}
			}
			// Check if any sub-expression contains a self-call (non-tail).
			ast.Inspect(s, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == funcName {
						*hasSelfCall = true
						*allTail = false
					}
				}
				return true
			})
		case *ast.IfStmt:
			if s.Init != nil {
				checkNonTailCalls(s.Init, funcName, hasSelfCall, allTail)
			}
			checkNonTailCallsExpr(s.Cond, funcName, hasSelfCall, allTail)
			inspectTailCalls(s.Body.List, funcName, hasSelfCall, allTail)
			if s.Else != nil {
				switch e := s.Else.(type) {
				case *ast.BlockStmt:
					inspectTailCalls(e.List, funcName, hasSelfCall, allTail)
				case *ast.IfStmt:
					inspectTailCalls([]ast.Stmt{e}, funcName, hasSelfCall, allTail)
				}
			}
		case *ast.BlockStmt:
			inspectTailCalls(s.List, funcName, hasSelfCall, allTail)
		default:
			// Any non-return, non-if statement: self-calls here are non-tail.
			// But only if this is NOT the last statement.
			checkNonTailCalls(s, funcName, hasSelfCall, allTail)
		}
	}
}

func checkNonTailCalls(node ast.Node, funcName string, hasSelfCall, allTail *bool) {
	ast.Inspect(node, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == funcName {
				*hasSelfCall = true
				*allTail = false
			}
		}
		return true
	})
}

func checkNonTailCallsExpr(node ast.Expr, funcName string, hasSelfCall, allTail *bool) {
	ast.Inspect(node, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == funcName {
				*hasSelfCall = true
				*allTail = false
			}
		}
		return true
	})
}
