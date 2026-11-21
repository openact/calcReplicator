// Package calcReplicator 提供"复刻模型计算"的引擎：把模型的公式清单（IR）
// 翻译成 Excel 公式，与模型 seriatim 输出逐格 diff。这是与具体模型无关的通用库。
package calcReplicator

import (
	"strconv"
	"strings"
)

// Node 是公式的中间表示（IR）。每个节点渲染成 Excel 表达式的一部分。
type Node struct {
	kind string // num | str | t | hdr | cur | prev | at | param | cfg | binop | fn | if | eread

	num  float64 // num
	str  string  // str
	name string  // cur/prev/at/param 的变量名
	at   *Node   // at 的时点索引（num）
	slot string  // cfg 的槽位：data/esg/sim/product/projTermM

	op   string // binop
	l, r *Node  // binop 左右
	fn   string // fn
	args []*Node

	table  string // eread 的字面表名；为空 + isESG=true 表示用 $B$29
	isESG  bool   // eread 是否 ESG（表名取配置单元格）
	coords []*Node

	cond, then_, else_ *Node // if
}

// —— 构造函数（供清单使用） ——

func Num(v float64) *Node       { return &Node{kind: "num", num: v} }
func Str(s string) *Node        { return &Node{kind: "str", str: s} }
func TVar() *Node               { return &Node{kind: "t"} }
func Hdr() *Node                { return &Node{kind: "hdr"} }
func Cur(name string) *Node     { return &Node{kind: "cur", name: name} }
func Prev(name string) *Node    { return &Node{kind: "prev", name: name} }
func At(name string, t *Node) *Node { return &Node{kind: "at", name: name, at: t} }
func Param(name string) *Node   { return &Node{kind: "param", name: name} }
func Cfg(slot string) *Node     { return &Node{kind: "cfg", slot: slot} }

func BinOp(op string, l, r *Node) *Node { return &Node{kind: "binop", op: op, l: l, r: r} }
func Fn(name string, args ...*Node) *Node {
	return &Node{kind: "fn", fn: name, args: args}
}
func If(c, t, e *Node) *Node { return &Node{kind: "if", cond: c, then_: t, else_: e} }

func Add(l, r *Node) *Node { return BinOp("+", l, r) }
func Sub(l, r *Node) *Node { return BinOp("-", l, r) }
func Mul(l, r *Node) *Node { return BinOp("*", l, r) }
func Div(l, r *Node) *Node { return BinOp("/", l, r) }
func Pow(l, r *Node) *Node { return BinOp("^", l, r) }
func Eq(l, r *Node) *Node  { return BinOp("=", l, r) }
func Gt(l, r *Node) *Node  { return BinOp(">", l, r) }

// ERead 表读取：ERead_File($B$data,".",<表>,<坐标...>)。
// 字面表名（Param.fac / Assump.fac）用 ERead；ESG（表名取 $B$esg）用 ESGRead。
func ERead(table string, coords ...*Node) *Node {
	return &Node{kind: "eread", table: table, coords: coords}
}
func ESGRead(coords ...*Node) *Node {
	return &Node{kind: "eread", isESG: true, coords: coords}
}

// renderCtx 是渲染上下文（工作簿的布局：行号映射、列名、配置/参数单元格）。
type renderCtx struct {
	row     map[string]int
	col     func(int) string
	paramR  map[string]int
	cfgR    map[string]int // slot -> 配置单元格行
	hdrRow  int
	colBase int
}

// render 把 IR 树渲染成 Excel 表达式（不含前导 =）。
func (n *Node) render(c *renderCtx, t int) string {
	switch n.kind {
	case "num":
		return strconv.FormatFloat(n.num, 'f', -1, 64)
	case "str":
		return `"` + n.str + `"`
	case "t":
		return strconv.Itoa(t)
	case "hdr":
		return c.col(c.colBase+t) + "$" + strconv.Itoa(c.hdrRow)
	case "cur":
		return c.col(c.colBase+t) + strconv.Itoa(c.row[n.name])
	case "prev":
		return c.col(c.colBase+t-1) + strconv.Itoa(c.row[n.name])
	case "at":
		idx := int(n.at.num)
		return c.col(c.colBase+idx) + strconv.Itoa(c.row[n.name])
	case "param":
		return "$B$" + strconv.Itoa(c.paramR[n.name])
	case "cfg":
		return "$B$" + strconv.Itoa(c.cfgR[n.slot])
	case "binop":
		return "(" + n.l.render(c, t) + n.op + n.r.render(c, t) + ")"
	case "fn":
		args := make([]string, len(n.args))
		for i, a := range n.args {
			args[i] = a.render(c, t)
		}
		return n.fn + "(" + strings.Join(args, ",") + ")"
	case "if":
		return "IF(" + n.cond.render(c, t) + "," + n.then_.render(c, t) + "," + n.else_.render(c, t) + ")"
	case "eread":
		table := n.table
		if n.isESG {
			table = "$B$" + strconv.Itoa(c.cfgR["esg"])
		} else {
			table = `"` + n.table + `"`
		}
		parts := []string{`ERead_File($B$` + strconv.Itoa(c.cfgR["data"]) + `,".",` + table}
		for _, cd := range n.coords {
			parts = append(parts, cd.render(c, t))
		}
		return strings.Join(parts, ",") + ")"
	}
	return ""
}
