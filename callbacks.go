package gorm

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	"gorm.io/gorm/schema"
	"gorm.io/gorm/utils"
)

// initializeCallbacks 各类 processor 的初始化是通过 initializeCallbacks 方法完成
func initializeCallbacks(db *DB) *callbacks {
	return &callbacks{
		processors: map[string]*processor{
			"create": {db: db},
			"query":  {db: db},
			"update": {db: db},
			"delete": {db: db},
			"row":    {db: db},
			"raw":    {db: db},
		},
	}
}

// callbacks gorm callbacks manager
// gorm 回调管理器
// 后续在请求执行过程中，会根据 crud 的类型，从 callbacks 中获取对应类型的 processor.
type callbacks struct {
	// 对应存储了 crud 等各类操作对应的执行器 processor
	// query -> query processor
	// create -> create processor
	// update -> update processor
	// delete -> delete processor
	// gorm 框架执行 crud 操作逻辑时使用到的执行器 processor
	// 针对 crud 操作的处理函数会以 list 的形式聚合在对应类型 processor 的 fns 字段当中.
	processors map[string]*processor
}

// processor gorm 框架执行 crud 操作逻辑时使用到的执行器 processor
type processor struct {
	// 从属的 DB 实例
	db *DB
	// 根据 crud 类型确定的 SQL 格式模板，后续用于拼接生成 sql
	// 拼接 sql 时的关键字顺序. 比如 query 类，固定为 SELECT,FROM,WHERE,GROUP BY, ORDER BY, LIMIT, FOR
	Clauses []string
	// 对应于 crud 类型的执行函数链
	// 针对 crud 操作的处理函数会以 list 的形式聚合在对应类型 processor 的 fns 字段当中.
	fns       []func(*DB)
	callbacks []*callback
}

type callback struct {
	name      string
	before    string
	after     string
	remove    bool
	replace   bool
	match     func(*DB) bool
	handler   func(*DB)
	processor *processor
}

func (cs *callbacks) Create() *processor {
	return cs.processors["create"]
}

// Query 一笔查询操作，会通过 callbacks.Query() 方法获取对应的 processor
func (cs *callbacks) Query() *processor {
	return cs.processors["query"]
}

func (cs *callbacks) Update() *processor {
	return cs.processors["update"]
}

func (cs *callbacks) Delete() *processor {
	return cs.processors["delete"]
}

func (cs *callbacks) Row() *processor {
	return cs.processors["row"]
}

func (cs *callbacks) Raw() *processor {
	return cs.processors["raw"]
}

// Execute 通用的 processor 执行函数，其中对应于 crud 的核心操作都被封装在 processor 对应的 fns list(函数处理链中) 当中了
func (p *processor) Execute(db *DB) *DB {
	// call scopes
	for len(db.Statement.scopes) > 0 {
		db = db.executeScopes()
	}

	var (
		curTime           = time.Now()
		stmt              = db.Statement
		resetBuildClauses bool
	)

	if len(stmt.BuildClauses) == 0 {
		// 根据 crud 类型，对 buildClauses 进行复制，用于后续的 sql 拼接
		stmt.BuildClauses = p.Clauses
		resetBuildClauses = true
	}

	if optimizer, ok := db.Statement.Dest.(StatementModifier); ok {
		optimizer.ModifyStatement(stmt)
	}

	// assign model values
	// dest 和 model 相互赋值
	if stmt.Model == nil {
		stmt.Model = stmt.Dest
	} else if stmt.Dest == nil {
		stmt.Dest = stmt.Model
	}

	// parse model values
	// 解析 model，获取对应表的 schema 信息(数据库表名信息和操作表的概要信息)
	if stmt.Model != nil {
		if err := stmt.Parse(stmt.Model); err != nil && (!errors.Is(err, schema.ErrUnsupportedDataType) || (stmt.Table == "" && stmt.TableExpr == nil && stmt.SQL.Len() == 0)) {
			if errors.Is(err, schema.ErrUnsupportedDataType) && stmt.Table == "" && stmt.TableExpr == nil {
				db.AddError(fmt.Errorf("%w: Table not set, please set it like: db.Model(&user) or db.Table(\"users\")", err))
			} else {
				db.AddError(err)
			}
		}
	}

	// assign stmt.ReflectValue
	// 处理 dest 信息（处理结果反序列化到此），将其添加到 stmt 当中
	if stmt.Dest != nil {
		stmt.ReflectValue = reflect.ValueOf(stmt.Dest)
		for stmt.ReflectValue.Kind() == reflect.Ptr {
			if stmt.ReflectValue.IsNil() && stmt.ReflectValue.CanAddr() {
				stmt.ReflectValue.Set(reflect.New(stmt.ReflectValue.Type().Elem()))
			}

			stmt.ReflectValue = stmt.ReflectValue.Elem()
		}
		if !stmt.ReflectValue.IsValid() {
			db.AddError(ErrInvalidValue)
		}
	}
	// 执行一系列的 callback 函数，其中最核心的 create/query/update/delete 操作都被包含在其中了.
	// 还包括了一系列前、后处理函数，对应于 crud 类型的执行函数链
	for _, f := range p.fns {
		f(db)
	}

	if stmt.SQL.Len() > 0 {
		db.Logger.Trace(stmt.Context, curTime, func() (string, int64) {
			sql, vars := stmt.SQL.String(), stmt.Vars
			if filter, ok := db.Logger.(ParamsFilter); ok {
				sql, vars = filter.ParamsFilter(stmt.Context, stmt.SQL.String(), stmt.Vars...)
			}
			return db.Dialector.Explain(sql, vars...), db.RowsAffected
		}, db.Error)
	}

	if !stmt.DB.DryRun {
		stmt.SQL.Reset()
		stmt.Vars = nil
	}

	if resetBuildClauses {
		stmt.BuildClauses = nil
	}

	return db
}

func (p *processor) Get(name string) func(*DB) {
	for i := len(p.callbacks) - 1; i >= 0; i-- {
		if v := p.callbacks[i]; v.name == name && !v.remove {
			return v.handler
		}
	}
	return nil
}

func (p *processor) Before(name string) *callback {
	return &callback{before: name, processor: p}
}

func (p *processor) After(name string) *callback {
	return &callback{after: name, processor: p}
}

func (p *processor) Match(fc func(*DB) bool) *callback {
	return &callback{match: fc, processor: p}
}

// Register 注册某个特定 fn 函数 的入口是 processor.Register 方法
func (p *processor) Register(name string, fn func(*DB)) error {
	return (&callback{processor: p}).Register(name, fn)
}

func (p *processor) Remove(name string) error {
	return (&callback{processor: p}).Remove(name)
}

func (p *processor) Replace(name string, fn func(*DB)) error {
	return (&callback{processor: p}).Replace(name, fn)
}

// compile 注册某个特定 fn 函数 的入口是 processor.Register 方法，
func (p *processor) compile() (err error) { // 编译
	var callbacks []*callback
	removedMap := map[string]bool{}
	for _, callback := range p.callbacks {
		if callback.match == nil || callback.match(p.db) {
			callbacks = append(callbacks, callback)
		}
		if callback.remove {
			removedMap[callback.name] = true
		}
	}

	if len(removedMap) > 0 {
		callbacks = removeCallbacks(callbacks, removedMap)
	}
	p.callbacks = callbacks

	if p.fns, err = sortCallbacks(p.callbacks); err != nil {
		p.db.Logger.Error(context.Background(), "Got error when compile callbacks, got %v", err)
	}
	return
}

func (c *callback) Before(name string) *callback {
	c.before = name
	return c
}

func (c *callback) After(name string) *callback {
	c.after = name
	return c
}

// Register 注册某个特定 fn 函数的入口是 processor.Register 方法，
func (c *callback) Register(name string, fn func(*DB)) error {
	c.name = name  // processor执行操作
	c.handler = fn // 函数处理链
	c.processor.callbacks = append(c.processor.callbacks, c)
	return c.processor.compile()
}

func (c *callback) Remove(name string) error {
	c.processor.db.Logger.Warn(context.Background(), "removing callback `%s` from %s\n", name, utils.FileWithLineNum())
	c.name = name
	c.remove = true
	c.processor.callbacks = append(c.processor.callbacks, c)
	return c.processor.compile()
}

func (c *callback) Replace(name string, fn func(*DB)) error {
	c.processor.db.Logger.Info(context.Background(), "replacing callback `%s` from %s\n", name, utils.FileWithLineNum())
	c.name = name
	c.handler = fn
	c.replace = true
	c.processor.callbacks = append(c.processor.callbacks, c)
	return c.processor.compile()
}

// getRIndex get right index from string slice
func getRIndex(strs []string, str string) int {
	for i := len(strs) - 1; i >= 0; i-- {
		if strs[i] == str {
			return i
		}
	}
	return -1
}

// Callbacks 排序
func sortCallbacks(cs []*callback) (fns []func(*DB), err error) {
	var (
		names, sorted []string
		sortCallback  func(*callback) error
	)
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[j].before == "*" && cs[i].before != "*" {
			return true
		}
		if cs[j].after == "*" && cs[i].after != "*" {
			return true
		}
		return false
	})

	for _, c := range cs {
		// show warning message the callback name already exists
		if idx := getRIndex(names, c.name); idx > -1 && !c.replace && !c.remove && !cs[idx].remove {
			c.processor.db.Logger.Warn(context.Background(), "duplicated callback `%s` from %s\n", c.name, utils.FileWithLineNum())
		}
		names = append(names, c.name)
	}

	sortCallback = func(c *callback) error {
		if c.before != "" { // if defined before callback
			if c.before == "*" && len(sorted) > 0 {
				if curIdx := getRIndex(sorted, c.name); curIdx == -1 {
					sorted = append([]string{c.name}, sorted...)
				}
			} else if sortedIdx := getRIndex(sorted, c.before); sortedIdx != -1 {
				if curIdx := getRIndex(sorted, c.name); curIdx == -1 {
					// if before callback already sorted, append current callback just after it
					sorted = append(sorted[:sortedIdx], append([]string{c.name}, sorted[sortedIdx:]...)...)
				} else if curIdx > sortedIdx {
					return fmt.Errorf("conflicting callback %s with before %s", c.name, c.before)
				}
			} else if idx := getRIndex(names, c.before); idx != -1 {
				// if before callback exists
				cs[idx].after = c.name
			}
		}

		if c.after != "" { // if defined after callback
			if c.after == "*" && len(sorted) > 0 {
				if curIdx := getRIndex(sorted, c.name); curIdx == -1 {
					sorted = append(sorted, c.name)
				}
			} else if sortedIdx := getRIndex(sorted, c.after); sortedIdx != -1 {
				if curIdx := getRIndex(sorted, c.name); curIdx == -1 {
					// if after callback sorted, append current callback to last
					sorted = append(sorted, c.name)
				} else if curIdx < sortedIdx {
					return fmt.Errorf("conflicting callback %s with before %s", c.name, c.after)
				}
			} else if idx := getRIndex(names, c.after); idx != -1 {
				// if after callback exists but haven't sorted
				// set after callback's before callback to current callback
				after := cs[idx]

				if after.before == "" {
					after.before = c.name
				}

				if err := sortCallback(after); err != nil {
					return err
				}

				if err := sortCallback(c); err != nil {
					return err
				}
			}
		}

		// if current callback haven't been sorted, append it to last
		if getRIndex(sorted, c.name) == -1 {
			sorted = append(sorted, c.name)
		}

		return nil
	}

	for _, c := range cs {
		if err = sortCallback(c); err != nil {
			return
		}
	}

	for _, name := range sorted {
		if idx := getRIndex(names, name); !cs[idx].remove {
			fns = append(fns, cs[idx].handler)
		}
	}

	return
}

func removeCallbacks(cs []*callback, nameMap map[string]bool) []*callback {
	callbacks := make([]*callback, 0, len(cs))
	for _, callback := range cs {
		if nameMap[callback.name] {
			continue
		}
		callbacks = append(callbacks, callback)
	}
	return callbacks
}
