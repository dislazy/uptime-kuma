package conditions

// Operator 是操作符的展示信息。前端只认 id，caption 用于下拉框文案。
type Operator struct {
	ID      string `json:"id"`
	Caption string `json:"caption"`
}

// Variable 是一个可在条件里引用的变量及其支持的操作符集合。
type Variable struct {
	ID        string     `json:"id"`
	Operators []Operator `json:"operators"`
}

// 与官方 defaultStringOperators 一一对应
var stringOperators = []Operator{
	{ID: "equals", Caption: "equals"},
	{ID: "not_equals", Caption: "not equals"},
	{ID: "contains", Caption: "contains"},
	{ID: "not_contains", Caption: "not contains"},
	{ID: "starts_with", Caption: "starts with"},
	{ID: "not_starts_with", Caption: "not starts with"},
	{ID: "ends_with", Caption: "ends with"},
	{ID: "not_ends_with", Caption: "not ends with"},
}

// 与官方 defaultNumberOperators 一一对应
var numberOperators = []Operator{
	{ID: "num_equals", Caption: "equals"},
	{ID: "num_not_equals", Caption: "not equals"},
	{ID: "lt", Caption: "less than"},
	{ID: "gt", Caption: "greater than"},
	{ID: "lte", Caption: "less than or equal to"},
	{ID: "gte", Caption: "greater than or equal to"},
}

// SupportFor 返回某个监控类型是否支持条件，以及可用的变量列表。
//
// 这份表决定前端是否显示「条件」折叠面板，必须与实际探测逻辑保持一致：
// 报 true 的类型，对应的 checker 就必须真的去求值。
func SupportFor(monitorType string) (bool, []Variable) {
	switch monitorType {
	case "dns":
		return true, []Variable{
			{ID: "record", Operators: cloneOperators(stringOperators)},
		}

	case "mqtt":
		// json_value 既可能是文本也可能是数字，所以两类操作符都给
		mixed := append(cloneOperators(stringOperators), cloneOperators(numberOperators)...)
		return true, []Variable{
			{ID: "topic", Operators: cloneOperators(stringOperators)},
			{ID: "message", Operators: cloneOperators(stringOperators)},
			{ID: "json_value", Operators: mixed},
		}

	case "mysql", "sqlserver", "mssql", "oracledb":
		return true, []Variable{
			{ID: "result", Operators: cloneOperators(stringOperators)},
		}
	}

	return false, []Variable{}
}

// cloneOperators 复制一份，避免调用方误改共享切片。
func cloneOperators(src []Operator) []Operator {
	out := make([]Operator, len(src))
	copy(out, src)
	return out
}
