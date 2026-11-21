package calcReplicator

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseDSL 把一条公式 DSL 字符串解析成 IR 树。
//
// 语法（递归下降）：
//   expr       := "if" expr "then" expr "else" expr | cmp
//   cmp        := add (("=="|">"|"<"|">="|"<="|"!=") add)*
//   add        := mul (("+"|"-") mul)*
//   mul        := unary (("*"|"/") unary)*
//   unary      := ("-") unary | postfix
//   postfix    := primary ("[" "-1" "]")?            // 仅支持 VAR[-1]（前一时点）
//   primary    := num | str | "@"ident | ident | ident"("args")" | "("expr")"
//
// 标识符约定：
//   t                    当前时点索引
//   date                 日期表头单元格
//   VAR                  变量当前时点（Cur）
//   VAR[-1]              变量前一时点（Prev）
//   next(VAR)            变量下一时点（At t+1，向后递归用）
//   yearend(VAR)         变量所在年的年末时点（At yearEndIdx）
//   @slot                配置/上下文引用（sim/product/projTermM/economy/category/startYear/startMonth…）
//   int/mod/round/max/min/pow   函数
//   eread("表", …)       字面表读取；eread(@esg, …) 表示 ESG（表名取配置单元格）
func ParseDSL(src string, ctx *Ctx) (*Node, error) {
	p := &dslParser{toks: lexDSL(src), ctx: ctx}
	n, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tokEOF {
		return nil, fmt.Errorf("公式末尾多余内容: %q", p.peek().val)
	}
	return n, nil
}

// —— 词法 ——

type tokKind int

const (
	tokEOF tokKind = iota
	tokNum
	tokStr
	tokIdent
	tokAtref // @ident
	tokOp
	tokLParen
	tokRParen
	tokLBracket
	tokRBracket
	tokComma
)

type token struct {
	kind tokKind
	val  string
}

func lexDSL(src string) []token {
	var toks []token
	i := 0
	for i < len(src) {
		ch := src[i]
		switch {
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r':
			i++
		case ch == '(':
			toks = append(toks, token{tokLParen, "("}); i++
		case ch == ')':
			toks = append(toks, token{tokRParen, ")"}); i++
		case ch == '[':
			toks = append(toks, token{tokLBracket, "["}); i++
		case ch == ']':
			toks = append(toks, token{tokRBracket, "]"}); i++
		case ch == ',':
			toks = append(toks, token{tokComma, ","}); i++
		case ch == '@':
			j := i + 1
			for j < len(src) && isIdentChar(src[j]) {
				j++
			}
			toks = append(toks, token{tokAtref, src[i+1 : j]}); i = j
		case ch == '\'' || ch == '"':
			q := ch
			j := i + 1
			for j < len(src) && src[j] != q {
				j++
			}
			toks = append(toks, token{tokStr, src[i+1 : j]})
			i = j + 1
		case isDigit(ch) || (ch == '.' && i+1 < len(src) && isDigit(src[i+1])):
			j := i
			for j < len(src) && (isDigit(src[j]) || src[j] == '.' || src[j] == 'e' || src[j] == 'E' || src[j] == '+' || src[j] == '-') {
				// 简单处理：数字后只允许 + - 出现在 e/E 之后；这里放宽，解析失败会报错
				if (src[j] == '+' || src[j] == '-') && j > i && src[j-1] != 'e' && src[j-1] != 'E' {
					break
				}
				j++
			}
			toks = append(toks, token{tokNum, src[i:j]}); i = j
		case isIdentChar(ch):
			j := i
			for j < len(src) && isIdentChar(src[j]) {
				j++
			}
			toks = append(toks, token{tokIdent, src[i:j]}); i = j
		default:
			// 操作符（最长匹配）
			two := ""
			if i+1 < len(src) {
				two = src[i : i+2]
			}
			if two == "==" || two == "!=" || two == ">=" || two == "<=" {
				toks = append(toks, token{tokOp, two}); i += 2
			} else if strings.ContainsRune("+-*/^><", rune(ch)) {
				toks = append(toks, token{tokOp, string(ch)}); i++
			} else {
				return append(toks, token{tokIdent, src[i:]}) // 未知字符，让解析器报错
			}
		}
	}
	toks = append(toks, token{tokEOF, ""})
	return toks
}

func isDigit(c byte) bool  { return c >= '0' && c <= '9' }
func isIdentChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
}

// —— 解析 ——

type dslParser struct {
	toks []token
	pos  int
	ctx  *Ctx
}

func (p *dslParser) peek() token { return p.toks[p.pos] }
func (p *dslParser) next() token { t := p.toks[p.pos]; p.pos++; return t }

func (p *dslParser) expect(k tokKind) (token, error) {
	t := p.next()
	if t.kind != k {
		return t, fmt.Errorf("期望 %v，得到 %q", k, t.val)
	}
	return t, nil
}

func (p *dslParser) parseExpr() (*Node, error) {
	// if ... then ... else
	if p.peek().kind == tokIdent && p.peek().val == "if" {
		p.next()
		cond, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if p.peek().kind != tokIdent || p.peek().val != "then" {
			return nil, fmt.Errorf("if 后缺少 then")
		}
		p.next()
		thenN, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if p.peek().kind != tokIdent || p.peek().val != "else" {
			return nil, fmt.Errorf("then 后缺少 else")
		}
		p.next()
		elseN, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		return If(cond, thenN, elseN), nil
	}
	return p.parseCmp()
}

func (p *dslParser) parseCmp() (*Node, error) {
	left, err := p.parseAdd()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tokOp {
		op := p.peek().val
		if op != "==" && op != "!=" && op != ">" && op != "<" && op != ">=" && op != "<=" {
			break
		}
		p.next()
		right, err := p.parseAdd()
		if err != nil {
			return nil, err
		}
		op2 := op
		if op == "==" {
			op2 = "=" // Excel 相等用单等号
		} else if op == "!=" {
			op2 = "<>"
		}
		left = BinOp(op2, left, right)
	}
	return left, nil
}

func (p *dslParser) parseAdd() (*Node, error) {
	left, err := p.parseMul()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tokOp && (p.peek().val == "+" || p.peek().val == "-") {
		op := p.next().val
		right, err := p.parseMul()
		if err != nil {
			return nil, err
		}
		left = BinOp(op, left, right)
	}
	return left, nil
}

func (p *dslParser) parseMul() (*Node, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tokOp && (p.peek().val == "*" || p.peek().val == "/" || p.peek().val == "^") {
		op := p.next().val
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = BinOp(op, left, right)
	}
	return left, nil
}

func (p *dslParser) parseUnary() (*Node, error) {
	if p.peek().kind == tokOp && p.peek().val == "-" {
		p.next()
		n, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		if n.kind == "num" {
			return Num(-n.num), nil
		}
		return BinOp("-", Num(0), n), nil
	}
	return p.parsePostfix()
}

func (p *dslParser) parsePostfix() (*Node, error) {
	n, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	// VAR[-1] → Prev
	if p.peek().kind == tokLBracket {
		p.next()
		if p.peek().kind == tokNum && p.peek().val == "-1" {
			p.next()
			p.next() // ]
			if n.kind != "cur" {
				return nil, fmt.Errorf("只有变量能带 [-1]")
			}
			return Prev(n.name), nil
		}
		if p.peek().kind == tokOp && p.peek().val == "-" {
			p.next()
			if p.peek().kind == tokNum && p.peek().val == "1" {
				p.next()
				p.next() // ]
				if n.kind != "cur" {
					return nil, fmt.Errorf("只有变量能带 [-1]")
				}
				return Prev(n.name), nil
			}
		}
		return nil, fmt.Errorf("仅支持 VAR[-1]；其他时点用 next(VAR) / yearend(VAR)")
	}
	return n, nil
}

func (p *dslParser) parsePrimary() (*Node, error) {
	t := p.next()
	switch t.kind {
	case tokNum:
		f, err := strconv.ParseFloat(t.val, 64)
		if err != nil {
			return nil, err
		}
		return Num(f), nil
	case tokStr:
		return Str(t.val), nil
	case tokAtref:
		return p.atref(t.val)
	case tokIdent:
		switch t.val {
		case "t":
			return TVar(), nil
		case "date":
			return Hdr(), nil
		case "nummonths":
			return &Node{kind: "nummonths"}, nil
		case "next", "yearend":
			if p.peek().kind != tokLParen {
				return nil, fmt.Errorf("%s 需要括号", t.val)
			}
			args, err := p.parseArgs()
			if err != nil {
				return nil, err
			}
			if len(args) != 1 || args[0].kind != "cur" {
				return nil, fmt.Errorf("%s 需要一个变量参数", t.val)
			}
			if t.val == "next" {
				return At(args[0].name, Add(TVar(), Num(1))), nil
			}
			return At(args[0].name, &Node{kind: "yearend"}), nil
		default:
			// 变量或函数
			if p.peek().kind == tokLParen {
				args, err := p.parseArgs()
				if err != nil {
					return nil, err
				}
				return p.fnCall(t.val, args)
			}
			return Cur(t.val), nil
		}
	case tokLParen:
		n, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		_, err = p.expect(tokRParen)
		return n, err
	}
	return nil, fmt.Errorf("意外的 token: %q", t.val)
}

func (p *dslParser) parseArgs() ([]*Node, error) {
	_, err := p.expect(tokLParen)
	if err != nil {
		return nil, err
	}
	var args []*Node
	if p.peek().kind == tokRParen {
		p.next()
		return args, nil
	}
	for {
		a, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		args = append(args, a)
		if p.peek().kind == tokComma {
			p.next()
			continue
		}
		break
	}
	_, err = p.expect(tokRParen)
	return args, err
}

func (p *dslParser) atref(name string) (*Node, error) {
	switch name {
	case "sim", "product", "projTermM", "data", "esg":
		return Cfg(name), nil
	case "economy":
		return Str(p.ctx.Economy), nil
	case "category":
		return Str(p.ctx.Category), nil
	case "startYear":
		return Num(float64(p.ctx.StartYear)), nil
	case "startMonth":
		return Num(float64(p.ctx.StartMonth)), nil
	}
	return nil, fmt.Errorf("未知的 @引用: %s", name)
}

func (p *dslParser) fnCall(name string, args []*Node) (*Node, error) {
	switch name {
	case "int":
		need(name, args, 1)
		return Fn("INT", args...), nil
	case "mod":
		need(name, args, 2)
		return Fn("MOD", args...), nil
	case "round":
		need(name, args, 2)
		return Fn("ROUND", args...), nil
	case "max":
		need(name, args, 2)
		return Fn("MAX", args...), nil
	case "min":
		need(name, args, 2)
		return Fn("MIN", args...), nil
	case "pow":
		need(name, args, 2)
		return BinOp("^", args[0], args[1]), nil
	case "eread":
		if len(args) < 1 {
			return nil, fmt.Errorf("eread 至少需要表名")
		}
		if args[0].kind == "cfg" && args[0].slot == "esg" {
			return ESGRead(args[1:]...), nil
		}
		if args[0].kind != "str" {
			return nil, fmt.Errorf("eread 表名须为字符串或 @esg")
		}
		return ERead(args[0].str, args[1:]...), nil
	case "param":
		if len(args) != 1 || args[0].kind != "cur" {
			return nil, fmt.Errorf("param 需要一个变量参数")
		}
		return Param(args[0].name), nil
	}
	return nil, fmt.Errorf("未知函数: %s", name)
}

func need(name string, args []*Node, n int) {
	if len(args) != n {
		panic(fmt.Sprintf("%s 需要 %d 个参数，得到 %d", name, n, len(args)))
	}
}
