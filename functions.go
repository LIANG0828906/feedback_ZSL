package downsample

import "strings"

// ResolveFunction follows reference aliases in either flat or nested-table
// normalized form and rejects reference cycles with the full field path.
func (t FunctionTable) Resolve(name string) (FunctionDef, *Error) {
	visited := map[string]bool{}
	path := make([]string, 0, 4)
	current := name
	for {
		if visited[current] {
			path = append(path, current)
			return FunctionDef{}, NewError(CodeFunctionCycle, "aggregate function reference cycle").
				WithFieldPath("functions."+strings.Join(path, ".ref.")).
				WithContext("cycle", append([]string(nil), path...))
		}
		def, ok := t.Defs[current]
		if !ok {
			return FunctionDef{}, NewError(CodeFunctionNotFound, "aggregate function is not declared").
				WithFieldPath("functions."+current).
				WithContext("function", name)
		}
		visited[current] = true
		path = append(path, current)
		if def.Ref == "" {
			return def, nil
		}
		current = def.Ref
	}
}

func ValidateFunctionTable(table FunctionTable) *Error {
	for name := range table.Defs {
		if _, err := table.Resolve(name); err != nil {
			return err
		}
	}
	return nil
}

func DefaultFunctionTable() FunctionTable {
	defs := map[string]FunctionDef{}
	for _, fn := range []Func{FuncCount, FuncSum, FuncAvg, FuncMin, FuncMax, FuncFirst, FuncLast, FuncWeightedAvg} {
		defs[string(fn)] = FunctionDef{Name: string(fn), Op: fn}
	}
	defs["mean_alias"] = FunctionDef{Name: "mean_alias", Ref: "avg"}
	return FunctionTable{Organization: "flat", Defs: defs}
}
