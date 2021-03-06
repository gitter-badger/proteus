package proteus

import (
	"bytes"
	"errors"
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/jonbodner/proteus/api"
	"github.com/jonbodner/proteus/mapper"
	"go/scanner"
	"go/token"
	"reflect"
	"strings"
)

/*
struct tags:
proq - Query run by Executor.Query. Returns single entity or list of entities
proe - Query run by Executor.Exec. Returns new id (if sql.Result has a non-zero value for LastInsertId) or number of rows changed
prop - The parameter names. Should be in order for the function parameters (skipping over the first Executor parameter)
prof - The fields on the dto that are mapped to select parameters in a query
next:
Put a reference to a public string instead of the query and that public string will be used as the query
later:
struct tags to mark as CRUD operations

converting name parameterized queries to positional queries
1. build a map of prop entry->position in parameter list
2. For each : in the input query
3. Find it
4. Find the end of the term (whitespace, comma, or end of string)
5. Create a map of querypos -> struct {parameter position (int), field path (string), isSlice (bool)}
6. If is slice, replace with ??
7. Otherwise, replace with ?
8. Capture the new string and the map in the closure
9. On run, do the replacements directly
10. If there are slices (??), replace with series of ? separated by commas, blow out slice in args slice

Return type is 0, 1, or 2 values
If zero, suppress all errors and return values (not great)
If 1:
For exec, return LastInsertId if !=0, otherwise return # of row changes (int in either case)
For query, if return type is []Entity, map to the entities.
For query, if return type is Entity and there are > 1 value, return the first. If there are zero, return the zero value of the Entity.
If 2:
Same as 1, 2nd parameter is error
Exception: if return type is entity and there are 0 values or > 1 value, return error indicating this.

On mapping for query, any unmappable parameters are ignored
If the entity is a primitive, then the first value returned for a row must be of that type, or it's an error. All other values for that row will be ignored.

*/

func validateFunction(funcType reflect.Type, isExec bool) error {
	//first parameter is Executor
	if funcType.NumIn() == 0 {
		return errors.New("Need to supply an Executor parameter")
	}
	exType := reflect.TypeOf((*api.Executor)(nil)).Elem()
	if !funcType.In(0).Implements(exType) {
		return errors.New("First parameter must be of type api.Executor")
	}
	//no in parameter can be a channel
	for i := 1; i < funcType.NumIn(); i++ {
		if funcType.In(i).Kind() == reflect.Chan {
			return errors.New("no input parameter can be a channel")
		}
	}

	//has 0, 1, or 2 return values
	if funcType.NumOut() > 2 {
		return errors.New("Must return 0, 1, or 2 values")
	}

	//if 2 return values, second is error
	if funcType.NumOut() == 2 {
		errType := reflect.TypeOf((*error)(nil)).Elem()
		if !funcType.Out(1).Implements(errType) {
			return errors.New("2nd output parameter must be of type error")
		}
	}

	//if 1 or 2, 1st param is not a channel (handle map, I guess)
	if funcType.NumOut() > 0 {
		if funcType.Out(0).Kind() == reflect.Chan {
			return errors.New("1st output parameter cannot be a channel")
		}
		if isExec && funcType.Out(0).Kind() != reflect.Int64 {
			return errors.New("The 1st output parameter of an Exec must be int64")
		}
	}
	return nil
}

type paramInfo struct {
	name        string
	posInParams int
}

// key == position in query
// value == name to evaluate & position in function in parameters
type queryParams []paramInfo

func buildQueryParams(posNameMap []string, paramMap map[string]int, funcType reflect.Type) (queryParams, error) {
	out := make(queryParams, len(posNameMap))
	for k, v := range posNameMap {
		//get just the first part of the name, before any .
		path := strings.SplitN(v, ".", 2)
		if pos, ok := paramMap[path[0]]; ok {
			//if the path has more than one part, make sure that the type of the function parameter is map or struct
			if len(path) > 1 {
				paramType := funcType.In(pos)
				switch paramType.Kind() {
				case reflect.Map, reflect.Struct:
					//do nothing
				default:
					return nil, fmt.Errorf("Query Parameter %s has a path, but the incoming parameter is not a map or a struct", v)
				}
			}
			out[k] = paramInfo{v, pos}
		} else {
			return nil, fmt.Errorf("Query Parameter %s cannot be found in the incoming parameters", v)
		}
	}

	//todo later: add support for slices as parameters
	return out, nil
}

func validIdentifier(curVar string) (string, error) {
	if strings.Contains(curVar, ";") {
		return "", fmt.Errorf("; is not allowed in an identifier: %s", curVar)
	}
	curVarB := []byte(curVar)

	var s scanner.Scanner
	fset := token.NewFileSet()                          // positions are relative to fset
	file := fset.AddFile("", fset.Base(), len(curVarB)) // register input "file"
	s.Init(file, curVarB, nil, scanner.Mode(0))

	lastPeriod := false
	first := true
	identifier := ""
loop:
	for {
		pos, tok, lit := s.Scan()
		log.Debugf("%s\t%s\t%q\n", fset.Position(pos), tok, lit)
		switch tok {
		case token.EOF:
			if first || lastPeriod {
				return "", fmt.Errorf("identifiers cannot be empty or end with a .: %s", curVar)
			}
			break loop
		case token.SEMICOLON:
			//happens with auto-insert from scanner
			//any explicit semicolons are illegal and handled earlier
			continue
		case token.IDENT:
			if !first && !lastPeriod {
				return "", fmt.Errorf(". missing between parts of an identifier: %s", curVar)
			}
			first = false
			lastPeriod = false
			identifier += lit
		case token.PERIOD:
			if first || lastPeriod {
				return "", fmt.Errorf("identifier cannot start with . or have two . in a row: %s", curVar)
			}
			lastPeriod = true
			identifier += "."
		default:
			return "", fmt.Errorf("Invalid character found in identifier: %s", curVar)
		}
	}
	return identifier, nil
}

func convertToPositionalParameters(query string, pa api.ParamAdapter) (string, []string, error) {
	var out bytes.Buffer

	posNames := []string{}

	// escapes:
	// \ (any character), that character literally (meant for escaping : and \)
	// ending on a single \ means the \ is ignored
	inEscape := false
	inVar := false
	curVar := []rune{}
	pos := 1
	for k, v := range query {
		if inEscape {
			out.WriteRune(v)
			inEscape = false
			continue
		}
		switch v {
		case '\\':
			inEscape = true
		case ':':
			if inVar {
				if len(curVar) == 0 {
					//error! must have a something
					return "", nil, fmt.Errorf("Empty variable declaration at position %d", k)
				}
				curVarS := string(curVar)
				id, err := validIdentifier(curVarS)
				if err != nil {
					//error, identifier must be valid go identifier with . for path
					return "", nil, err
				}
				posNames = append(posNames, id)
				inVar = false
				curVar = []rune{}
				out.WriteString(pa(pos))
				pos++
			} else {
				inVar = true
			}
		default:
			if inVar {
				curVar = append(curVar, v)
			} else {
				out.WriteRune(v)
			}
		}
	}
	if inVar {
		return "", nil, fmt.Errorf("Missing a closing : somewhere: %s", query)
	}
	//todo later: add support for slices as parameters
	return out.String(), posNames, nil
}

func getExecAndQArgs(args []reflect.Value, qps queryParams) (api.Executor, []interface{}, error) {
	//pull out that first input parameter as an Executor
	exec := args[0].Interface().(api.Executor)

	//walk through the rest of the input parameters and build a slice for args

	qArgs := make([]interface{}, len(qps))
	for k, v := range qps {
		//todo later: add support for slices as parameters
		val, err := mapper.Extract(args[v.posInParams].Interface(), strings.Split(v.name, "."))
		if err != nil {
			return nil, nil, err
		}
		qArgs[k] = val
	}

	return exec, qArgs, nil
}

func buildExec(funcType reflect.Type, query string, paramMap map[string]int, pa api.ParamAdapter) (func(args []reflect.Value) []reflect.Value, error) {
	positionalQuery, posNames, err := convertToPositionalParameters(query, pa)
	if err != nil {
		return nil, err
	}
	qps, err := buildQueryParams(posNames, paramMap, funcType)
	if err != nil {
		return nil, err
	}
	numOut := funcType.NumOut()
	return func(args []reflect.Value) []reflect.Value {
		exec, qArgs, err := getExecAndQArgs(args, qps)
		var result api.Result
		if err == nil {
			//call executor.Exec with query and parameters
			log.Debugln("calling", positionalQuery, "with params", qArgs)
			result, err = exec.Exec(positionalQuery, qArgs...)
		}

		//handle the 0,1,2 out parameter cases
		if numOut == 0 {
			return []reflect.Value{}
		}

		zero := reflect.ValueOf(int64(0))
		sType := funcType.Out(0)
		if numOut == 1 {
			if err != nil {
				return []reflect.Value{zero}
			}
			val, err := result.LastInsertId()
			if err != nil {
				return []reflect.Value{zero}
			}
			if val != 0 {
				return []reflect.Value{reflect.ValueOf(val).Convert(sType)}
			}
			val, err = result.RowsAffected()
			if err != nil {
				return []reflect.Value{zero}
			}
			return []reflect.Value{reflect.ValueOf(val).Convert(sType)}
		}
		if numOut == 2 {
			eType := funcType.Out(1)
			if err != nil {
				return []reflect.Value{zero, reflect.ValueOf(err).Convert(eType)}
			}
			val, err := result.LastInsertId()
			if err != nil {
				return []reflect.Value{zero, reflect.ValueOf(err).Convert(eType)}
			}
			if val != 0 {
				return []reflect.Value{reflect.ValueOf(val).Convert(sType), errZero}
			}
			val, err = result.RowsAffected()
			if err != nil {
				return []reflect.Value{zero, reflect.ValueOf(err).Convert(eType)}
			}
			return []reflect.Value{reflect.ValueOf(val).Convert(sType), errZero}
		}
		return []reflect.Value{zero, reflect.ValueOf(errors.New("Should never get here!"))}
	}, nil
}

func buildQuery(funcType reflect.Type, query string, paramMap map[string]int, pa api.ParamAdapter) (func(args []reflect.Value) []reflect.Value, error) {
	positionalQuery, posNames, err := convertToPositionalParameters(query, pa)
	if err != nil {
		return nil, err
	}
	qps, err := buildQueryParams(posNames, paramMap, funcType)
	if err != nil {
		return nil, err
	}
	numOut := funcType.NumOut()
	return func(args []reflect.Value) []reflect.Value {
		exec, qArgs, err := getExecAndQArgs(args, qps)

		var rows api.Rows
		//call executor.Query with query and parameters
		if err == nil {
			log.Debugln("calling", positionalQuery, "with params", qArgs)
			rows, err = exec.Query(positionalQuery, qArgs...)
		}

		//handle the 0,1,2 out parameter cases
		if numOut == 0 {
			return []reflect.Value{}
		}

		sType := funcType.Out(0)
		zero := reflect.Zero(sType)
		if numOut == 1 {
			if err != nil {
				return []reflect.Value{zero}
			}
			// handle mapping
			val, err := handleMapping(sType, rows)
			if err != nil {
				return []reflect.Value{zero}
			}
			return []reflect.Value{reflect.ValueOf(val).Convert(sType)}
		}
		if numOut == 2 {
			eType := funcType.Out(1)
			if err != nil {
				return []reflect.Value{zero, reflect.ValueOf(err).Convert(eType)}
			}
			// handle mapping
			val, err := handleMapping(sType, rows)
			var eVal reflect.Value
			if err == nil {
				eVal = errZero
			} else {
				eVal = reflect.ValueOf(err).Convert(eType)
			}
			return []reflect.Value{reflect.ValueOf(val).Convert(sType), eVal}
		}
		return []reflect.Value{zero, reflect.ValueOf(errors.New("Should never get here!"))}
	}, nil
}

var errZero = reflect.Zero(reflect.TypeOf((*error)(nil)).Elem())

func handleMapping(sType reflect.Type, rows api.Rows) (interface{}, error) {
	var val interface{}
	var err error
	if sType.Kind() == reflect.Slice {
		s := reflect.MakeSlice(sType, 0, 0)
		var result interface{}
		for {
			result, err = mapper.Map(rows, sType.Elem())
			if result == nil {
				break
			}
			s = reflect.Append(s, reflect.ValueOf(result))
		}
		val = s.Interface()
	} else {
		val, err = mapper.Map(rows, sType)
	}
	return val, err
}

func buildParamMap(prop string) map[string]int {
	queryParams := strings.Split(prop, ",")
	m := map[string]int{}
	for k, v := range queryParams {
		m[v] = k + 1
	}
	return m
}

// Build is the main entry point into Proteus
func Build(dao interface{}, pa api.ParamAdapter) error {
	t := reflect.TypeOf(dao)
	//must be a pointer to struct
	if t.Kind() != reflect.Ptr {
		return errors.New("Not a pointer")
	}
	t2 := t.Elem()
	//if not a struct, panic
	if t2.Kind() != reflect.Struct {
		return errors.New("Not a pointer to struct")
	}
	svp := reflect.ValueOf(dao)
	sv := reflect.Indirect(svp)
	//for each field in ProductDao that is of type func and has a proteus struct tag, assign it a func
	for i := 0; i < t2.NumField(); i++ {
		curField := t2.Field(i)
		proq := curField.Tag.Get("proq")
		proe := curField.Tag.Get("proe")
		prop := curField.Tag.Get("prop")
		if curField.Type.Kind() == reflect.Func && (proq != "" || proe != "") {
			//validate to make sure that the function matches what we expect
			err := validateFunction(curField.Type, proe != "")
			if err != nil {
				log.Warnln("skipping function", curField.Name, "due to error:", err.Error())
				continue
			}

			paramMap := buildParamMap(prop)

			var toFunc func(args []reflect.Value) []reflect.Value
			if proq != "" {
				toFunc, err = buildQuery(curField.Type, proq, paramMap, pa)
			} else {
				toFunc, err = buildExec(curField.Type, proe, paramMap, pa)
			}
			if err != nil {
				log.Warnln("skipping function", curField.Name, "due to error:", err.Error())
				continue
			}
			fv := sv.FieldByName(curField.Name)
			fv.Set(reflect.MakeFunc(curField.Type, toFunc))
		}
	}
	return nil
}
