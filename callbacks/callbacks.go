package callbacks

import (
	"gorm.io/gorm"
)

// 根据 crud 的类型，获取 sql 拼接格式 clauses，将其赋值到该 processor 的 BuildClauses 字段当中.
// crud 各类 clauses 格式展示如下
var (
	createClauses = []string{"INSERT", "VALUES", "ON CONFLICT"}
	queryClauses  = []string{"SELECT", "FROM", "WHERE", "GROUP BY", "ORDER BY", "LIMIT", "FOR"}
	updateClauses = []string{"UPDATE", "SET", "WHERE"}
	deleteClauses = []string{"DELETE", "FROM", "WHERE"}
)

type Config struct {
	LastInsertIDReversed bool
	CreateClauses        []string
	QueryClauses         []string
	UpdateClauses        []string
	DeleteClauses        []string
}

// RegisterDefaultCallbacks 注册默认callbacks管理器
// 对应于 crud 四类 processor，注册的函数链 fns 的内容和顺序是固定的
func RegisterDefaultCallbacks(db *gorm.DB, config *Config) {
	enableTransaction := func(db *gorm.DB) bool {
		return !db.SkipDefaultTransaction
	}

	if len(config.CreateClauses) == 0 {
		config.CreateClauses = createClauses
	}
	if len(config.QueryClauses) == 0 {
		config.QueryClauses = queryClauses
	}
	if len(config.DeleteClauses) == 0 {
		config.DeleteClauses = deleteClauses
	}
	if len(config.UpdateClauses) == 0 {
		config.UpdateClauses = updateClauses
	}

	//  创建类 create processor
	createCallback := db.Callback().Create()
	createCallback.Match(enableTransaction).Register("gorm:begin_transaction", BeginTransaction)
	createCallback.Register("gorm:before_create", BeforeCreate)
	createCallback.Register("gorm:save_before_associations", SaveBeforeAssociations(true))
	// 在 create 类型 processor 的 fns 函数链中，最主要的执行函数就是 Create
	createCallback.Register("gorm:create", Create(config))
	createCallback.Register("gorm:save_after_associations", SaveAfterAssociations(true))
	createCallback.Register("gorm:after_create", AfterCreate)
	createCallback.Match(enableTransaction).Register("gorm:commit_or_rollback_transaction", CommitOrRollbackTransaction)
	createCallback.Clauses = config.CreateClauses
	// 查询类 query processor
	queryCallback := db.Callback().Query()
	// 在 query 类型 processor（执行器） 的 fns 函数链中，最主要的函数是 Query
	queryCallback.Register("gorm:query", Query) // 将Query函数注册到 query 类型 processor（执行器）的 fns 函数链中
	queryCallback.Register("gorm:preload", Preload)
	queryCallback.Register("gorm:after_query", AfterQuery)
	queryCallback.Clauses = config.QueryClauses
	// 删除类 delete processorzhuc
	deleteCallback := db.Callback().Delete()
	deleteCallback.Match(enableTransaction).Register("gorm:begin_transaction", BeginTransaction)
	deleteCallback.Register("gorm:before_delete", BeforeDelete)
	deleteCallback.Register("gorm:delete_before_associations", DeleteBeforeAssociations)
	// 在 delete 类型的 processor 的 fns 函数链中，最核心的函数是 Delete
	deleteCallback.Register("gorm:delete", Delete(config))
	deleteCallback.Register("gorm:after_delete", AfterDelete)
	deleteCallback.Match(enableTransaction).Register("gorm:commit_or_rollback_transaction", CommitOrRollbackTransaction)
	deleteCallback.Clauses = config.DeleteClauses
	// 更新类 update processor
	updateCallback := db.Callback().Update()
	updateCallback.Match(enableTransaction).Register("gorm:begin_transaction", BeginTransaction)
	updateCallback.Register("gorm:setup_reflect_value", SetupUpdateReflectValue)
	updateCallback.Register("gorm:before_update", BeforeUpdate)
	updateCallback.Register("gorm:save_before_associations", SaveBeforeAssociations(false))
	// 在 update 类型 processor 的 fns 函数链中，最核心的函数就是 Update
	updateCallback.Register("gorm:update", Update(config))
	updateCallback.Register("gorm:save_after_associations", SaveAfterAssociations(false))
	updateCallback.Register("gorm:after_update", AfterUpdate)
	updateCallback.Match(enableTransaction).Register("gorm:commit_or_rollback_transaction", CommitOrRollbackTransaction)
	updateCallback.Clauses = config.UpdateClauses
	// row 类
	rowCallback := db.Callback().Row()
	rowCallback.Register("gorm:row", RowQuery)
	rowCallback.Clauses = config.QueryClauses
	// raw 类
	rawCallback := db.Callback().Raw()
	rawCallback.Register("gorm:raw", RawExec)
	rawCallback.Clauses = config.QueryClauses
}
