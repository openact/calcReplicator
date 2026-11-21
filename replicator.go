package calcReplicator

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"
)

// Ctx 是清单构建上下文（模型点属性 + 投影参数）。
type Ctx struct {
	Economy, Category               string
	ProjTermM, StartYear, StartMonth int
}

// YearEndIdx 返回时点 t 所在年的年末索引（tIdx(startYear,startMonth,yr,12)）。
func (c *Ctx) YearEndIdx(t int) int {
	yr := c.StartYear + (c.StartMonth-1+t)/12
	return (yr-c.StartYear)*12 + (12 - c.StartMonth)
}

// NumMonths 返回 ANN_INV_RET 年化月数（首年 12-StartMonth，其余 12）。
func (c *Ctx) NumMonths(t int) int {
	yr := c.StartYear + (c.StartMonth-1+t)/12
	if yr == c.StartYear {
		return 12 - c.StartMonth
	}
	return 12
}

// Manifest 是公式清单：变量名 -> 在时点 t 构建 IR 树（带上下文）。
type Manifest map[string]func(t int, c *Ctx) *Node

// MPF 是模型点的一行（从 mpf CSV 读取）。
type MPF struct {
	ID, PlanCode, SPCode, Economy, Category string
	ProjTermM                                int
}

// Options 是 Run 的输入。
type Options struct {
	Product, Run, ESG, Sim, Structure string
	InputDir, DataDir, OutputDir      string
	OutputFile                        string // 空则默认 replication-<product>.xlsx
	Variables, Params                 []string
	Manifest                          Manifest
	StartYear, StartMonth             int
	ProjMonths                        int
}

// Run 生成复刻工作簿：读 mpf + seriatim，用清单渲染 Excel 公式，输出 计算/期望/diff 三表。
func Run(opts Options) error {
	if opts.ProjMonths == 0 {
		opts.ProjMonths = 240
	}
	if opts.StartYear == 0 {
		opts.StartYear = 2026
	}
	if opts.StartMonth == 0 {
		opts.StartMonth = 6
	}
	if opts.OutputFile == "" {
		opts.OutputFile = "replication-" + opts.Product + ".xlsx"
	}

	mp := loadMPF(filepath.Join(opts.InputDir, "mpfs", "SubFund.csv"), opts.Product)
	ctx := &Ctx{
		Economy:    mp.Economy,
		Category:   mp.Category,
		ProjTermM:  mp.ProjTermM,
		StartYear:  opts.StartYear,
		StartMonth: opts.StartMonth,
	}
	seriatim := filepath.Join(opts.DataDir, opts.Run, "seriatim",
		fmt.Sprintf("%s-%s-%s-sp%s-sim%s.fac", opts.Structure, mp.ID, mp.PlanCode, mp.SPCode, opts.Sim))
	modelOut := loadSeriatim(seriatim, opts.Variables, opts.ProjMonths)

	if err := os.MkdirAll(opts.OutputDir, 0755); err != nil {
		return err
	}
	outPath := filepath.Join(opts.OutputDir, opts.OutputFile)
	return build(opts, ctx, mp, modelOut, outPath)
}

func loadMPF(path, product string) MPF {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开 mpf 失败: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		fmt.Fprintf(os.Stderr, "读 mpf 失败: %v\n", err)
		os.Exit(1)
	}
	for _, rec := range recs[2:] {
		if len(rec) < 7 {
			continue
		}
		if rec[1] == product {
			term, _ := strconv.Atoi(rec[6])
			return MPF{ID: rec[0], PlanCode: rec[1], SPCode: rec[2], Economy: rec[4], Category: rec[5], ProjTermM: term}
		}
	}
	fmt.Fprintf(os.Stderr, "mpf 中找不到产品 %s\n", product)
	os.Exit(1)
	return MPF{}
}

func loadSeriatim(path string, variables []string, projMonths int) map[string][]string {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开 seriatim 失败: %v（请先运行模型）\n", err)
		os.Exit(1)
	}
	defer f.Close()
	want := map[string]bool{}
	for _, v := range variables {
		want[v] = true
	}
	out := map[string][]string{}
	br := newLineReader(f)
	for {
		line, ok := br.readLine()
		if !ok {
			break
		}
		if !strings.HasPrefix(line, "*,") {
			continue
		}
		c := strings.Split(line, ",")
		if len(c) < 8 {
			continue
		}
		name := c[6]
		if !want[name] {
			continue
		}
		if _, done := out[name]; done {
			continue
		}
		out[name] = c[7 : 7+projMonths+1]
	}
	if len(out) < len(variables) {
		fmt.Fprintf(os.Stderr, "seriatim 变量不全: 期望 %d, 实得 %d\n", len(variables), len(out))
		os.Exit(1)
	}
	return out
}

type lineReader struct {
	r   io.Reader
	buf []byte
}

func newLineReader(r io.Reader) *lineReader { return &lineReader{r: r} }

func (l *lineReader) readLine() (string, bool) {
	l.buf = l.buf[:0]
	tmp := make([]byte, 1)
	for {
		n, err := l.r.Read(tmp)
		if n > 0 {
			if tmp[0] == '\n' {
				return string(l.buf), true
			}
			if tmp[0] != '\r' {
				l.buf = append(l.buf, tmp[0])
			}
		}
		if err != nil {
			if len(l.buf) > 0 {
				return string(l.buf), true
			}
			return "", false
		}
	}
}

func build(opts Options, ctx *Ctx, mp MPF, modelOut map[string][]string, outPath string) error {
	f := excelize.NewFile()
	defer f.Close()

	bold, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	section, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true, Color: "1F4E78"}})
	cfg, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Color: "1F6FEB"}})

	set := func(sheet, cell string, v any) {
		if err := f.SetCellValue(sheet, cell, v); err != nil {
			panic(err)
		}
	}
	fx := func(sheet, cell, formula string) {
		if err := f.SetCellFormula(sheet, cell, formula); err != nil {
			panic(err)
		}
	}
	sty := func(sheet, cell string, s int) {
		if err := f.SetCellStyle(sheet, cell, cell, s); err != nil {
			panic(err)
		}
	}
	colName := func(col int) string { n, _ := excelize.ColumnNumberToName(col); return n }

	const colBase = 2
	nCols := opts.ProjMonths + 1

	// ===== 计算 sheet =====
	calc := "计算"
	f.NewSheet(calc)
	hdrRow := 1
	row := map[string]int{}
	set(calc, "A1", "变量")
	sty(calc, "A1", bold)
	for t := 0; t < nCols; t++ {
		col := colBase + t
		yr := ctx.StartYear + (ctx.StartMonth-1+t)/12
		mth := (ctx.StartMonth-1+t)%12 + 1
		cell := fmt.Sprintf("%s%d", colName(col), hdrRow)
		set(calc, cell, yr*100+mth)
		sty(calc, cell, bold)
	}
	for i, v := range opts.Variables {
		r := 2 + i
		set(calc, fmt.Sprintf("A%d", r), v)
		row[v] = r
	}

	cfgRow := 27
	set(calc, fmt.Sprintf("A%d", cfgRow), "【配置与参数】(下方可改，改后 F9)")
	sty(calc, fmt.Sprintf("A%d", cfgRow), section)
	cfgCells := []struct {
		slot, name string
		val        any
	}{
		{"data", "数据目录 (resVault)", filepath.Join(opts.InputDir, "tables", "classG")},
		{"esg", "ESG 文件名", opts.ESG},
		{"sim", "sim", opts.Sim},
		{"product", "产品 (PLAN_CODE)", opts.Product},
		{"projTermM", "PROJ_TERM_M (mpf)", ctx.ProjTermM},
	}
	cfgR := map[string]int{}
	for i, c := range cfgCells {
		r := cfgRow + 1 + i
		set(calc, fmt.Sprintf("A%d", r), c.name)
		set(calc, fmt.Sprintf("B%d", r), c.val)
		sty(calc, fmt.Sprintf("B%d", r), cfg)
		cfgR[c.slot] = r
	}
	paramR := map[string]int{}
	paramHdr := cfgRow + 7
	set(calc, fmt.Sprintf("A%d", paramHdr), "【参数】Param.fac (ERead_File)")
	sty(calc, fmt.Sprintf("A%d", paramHdr), section)
	for i, name := range opts.Params {
		r := paramHdr + 1 + i
		set(calc, fmt.Sprintf("A%d", r), name)
		fx(calc, fmt.Sprintf("B%d", r), fmt.Sprintf(`=ERead_File($B$%d,".","Param.fac",$B$%d,"%s")`, cfgR["data"], cfgR["product"], name))
		paramR[name] = r
	}

	rc := &renderCtx{row: row, col: colName, paramR: paramR, cfgR: cfgR, hdrRow: hdrRow, colBase: colBase}
	for name, r := range row {
		builder, ok := opts.Manifest[name]
		if !ok {
			return fmt.Errorf("清单缺少变量 %s", name)
		}
		for t := 0; t < nCols; t++ {
			cn := colName(colBase + t)
			cell := fmt.Sprintf("%s%d", cn, r)
			fx(calc, cell, "="+builder(t, ctx).render(rc, t))
		}
	}

	// ===== 期望 sheet =====
	exp := "期望"
	f.NewSheet(exp)
	set(exp, "A1", "变量")
	sty(exp, "A1", bold)
	for t := 0; t < nCols; t++ {
		col := colBase + t
		yr := ctx.StartYear + (ctx.StartMonth-1+t)/12
		mth := (ctx.StartMonth-1+t)%12 + 1
		cell := fmt.Sprintf("%s%d", colName(col), 1)
		set(exp, cell, yr*100+mth)
		sty(exp, cell, bold)
	}
	for i, v := range opts.Variables {
		r := 2 + i
		set(exp, fmt.Sprintf("A%d", r), v)
		vals := modelOut[v]
		for t := 0; t < nCols && t < len(vals); t++ {
			fv, _ := strconv.ParseFloat(vals[t], 64)
			set(exp, fmt.Sprintf("%s%d", colName(colBase+t), r), fv)
		}
	}

	// ===== diff sheet =====
	diff := "diff"
	f.NewSheet(diff)
	set(diff, "A1", "变量")
	sty(diff, "A1", bold)
	for t := 0; t < nCols; t++ {
		col := colBase + t
		yr := ctx.StartYear + (ctx.StartMonth-1+t)/12
		mth := (ctx.StartMonth-1+t)%12 + 1
		cell := fmt.Sprintf("%s%d", colName(col), 1)
		set(diff, cell, yr*100+mth)
		sty(diff, cell, bold)
	}
	for i, v := range opts.Variables {
		r := 2 + i
		set(diff, fmt.Sprintf("A%d", r), v)
		for t := 0; t < nCols; t++ {
			cn := colName(colBase + t)
			fx(diff, fmt.Sprintf("%s%d", cn, r), fmt.Sprintf("='%s'!%s%d-'%s'!%s%d", calc, cn, r, exp, cn, r))
		}
	}

	f.DeleteSheet("Sheet1")
	f.SetActiveSheet(0)
	for _, s := range []string{calc, exp, diff} {
		f.SetColWidth(s, "A", "A", 24)
		f.SetColWidth(s, "B", "B", 14)
		f.SetColWidth(s, "C", "C", 14)
	}
	return f.SaveAs(outPath)
}
