package gorm

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"sync"
)

// Stmt 类是 gorm 框架对 database/sql 标准库下 Stmt 类的简单封装
type Stmt struct {
	// database/sql 标准库下的 statement
	*sql.Stmt
	// 是否处于事务
	Transaction bool
	// 标识当前 stmt 是否已初始化完成
	prepared   chan struct{}
	prepareErr error
}

// PreparedStmtDB prepare 模式下的 connPool 实现类.
// 该类的目的是为了使用 database/sql 标准库中的 prepare 能力，完成预处理状态 statement 的构造和复用.
type PreparedStmtDB struct {
	// 各 stmt 实例. 其中 key 为 sql 模板，stmt 是对封 database/sql 中 *Stmt 的封装
	Stmts map[string]*Stmt
	Mux   *sync.RWMutex
	// 内置的 ConnPool 字段通常为 database/sql 中的 *DB
	ConnPool
}

func NewPreparedStmtDB(connPool ConnPool) *PreparedStmtDB {
	return &PreparedStmtDB{
		ConnPool: connPool, // 内置的 ConnPool 字段通常为 database/sql 中的 *DB
		Stmts:    make(map[string]*Stmt),
		Mux:      &sync.RWMutex{}, // 读写锁
	}
}

func (db *PreparedStmtDB) GetDBConn() (*sql.DB, error) {
	if sqldb, ok := db.ConnPool.(*sql.DB); ok {
		return sqldb, nil
	}

	if dbConnector, ok := db.ConnPool.(GetDBConnector); ok && dbConnector != nil {
		return dbConnector.GetDBConn()
	}

	return nil, ErrInvalidDB
}

func (db *PreparedStmtDB) Close() {
	db.Mux.Lock()
	defer db.Mux.Unlock()

	for _, stmt := range db.Stmts {
		go func(s *Stmt) {
			// make sure the stmt must finish preparation first
			<-s.prepared
			if s.Stmt != nil {
				_ = s.Close()
			}
		}(stmt)
	}
	// setting db.Stmts to nil to avoid further using
	db.Stmts = nil
}

func (sdb *PreparedStmtDB) Reset() {
	sdb.Mux.Lock()
	defer sdb.Mux.Unlock()

	for _, stmt := range sdb.Stmts {
		go func(s *Stmt) {
			// make sure the stmt must finish preparation first
			<-s.prepared
			if s.Stmt != nil {
				_ = s.Close()
			}
		}(stmt)
	}
	sdb.Stmts = make(map[string]*Stmt)
}

// 在 PreparedStmtDB.prepare 方法中，会通过加锁 double check 的方式，创建或复用 sql 模板对应的 stmt
func (db *PreparedStmtDB) prepare(ctx context.Context, conn ConnPool, isTransaction bool, query string) (Stmt, error) {
	// 这个读锁的目的是保证在检查已有预处理语句（stmt）的时候，不会被其他正在进行写操作（比如初始化新的 stmt）的 goroutine 干扰
	db.Mux.RLock() // 读锁
	// 多个 goroutine 可以同时持有读锁，所以在这个阶段可以有多个 goroutine 同时检查是否存在满足条件的已有 stmt
	// 以 sql 模板为 key，优先复用已有的 stmt
	// 查看是否已经有对应的预处理语句存在于db.Stmts这个映射中。检查当前操作是否是事务操作或者已有的 stmt 不是事务操作
	if stmt, ok := db.Stmts[query]; ok && (!stmt.Transaction || isTransaction) {
		// 如果找到了满足条件的已有 stmt，说明可以直接复用,这时执行db.Mux.RUnlock()释放读锁，直接返回找到的 stmt
		db.Mux.RUnlock()
		// 并发场景下，只允许有一个 goroutine 完成 stmt 的初始化操作
		// wait for other goroutines prepared
		<-stmt.prepared
		if stmt.prepareErr != nil {
			return Stmt{}, stmt.prepareErr
		}
		return *stmt, nil
	}
	// 如果没有找到满足条件的已有 stmt，说明需要进行初始化操作
	// 两个连续的db.Mux.RUnlock()释放读锁，让其他 goroutine 可以继续执行后续代码。
	db.Mux.RUnlock()

	// 倘若 stmt 不存在，则加写锁 double check
	// 加锁 double check，确认未完成 stmt 初始化则执行初始化操作
	db.Mux.Lock()
	// double check
	if stmt, ok := db.Stmts[query]; ok && (!stmt.Transaction || isTransaction) {
		db.Mux.Unlock()
		// wait for other goroutines prepared
		// 当这个 goroutine 完成初始化后，会向stmt.prepared通道发送一个值。
		<-stmt.prepared // 通知其他正在等待的 goroutine 初始化已经完成
		// 因为第一个 goroutine 发送的值而被唤醒,其他 goroutine 恢复执行后，首先检查stmt.prepareErr是否为nil。
		if stmt.prepareErr != nil {
			return Stmt{}, stmt.prepareErr
		}

		return *stmt, nil
	}
	// check db.Stmts first to avoid Segmentation Fault(setting value to nil map)
	// which cause by calling Close and executing SQL concurrently
	// 首先检查db.Stmts，以避免并发调用Close和执行SQL导致的分割错误(将值设置为nil map)
	if db.Stmts == nil {
		db.Mux.Unlock()
		return Stmt{}, ErrInvalidDB
	}

	// 在所有等待的 goroutine 中，只有第一个被调度执行的 goroutine 会继续执行初始化操作
	// cache preparing stmt first
	// 创建 stmt 实例（gorm.Stmt），并添加到 stmts map 中
	// 这个实例还没有真正的（*sql.Stmt）
	cacheStmt := Stmt{Transaction: isTransaction, prepared: make(chan struct{})}
	db.Stmts[query] = &cacheStmt
	// 此时可以提前解锁是因为还通过 channel 保证了其他使用者会阻塞等待初始化操作完成
	db.Mux.Unlock()

	// prepare completed
	// 所有工作执行完之后会关闭 channel，唤醒其他阻塞等待使用 stmt 的 goroutine
	defer close(cacheStmt.prepared)

	// Reason why cannot lock conn.PrepareContext
	// suppose the maxopen is 1, g1 is creating record and g2 is querying record.
	// 1. g1 begin tx, g1 is requeue because of waiting for the system call(系统呼叫), now `db.ConnPool` db.numOpen == 1.
	// 2. g2 select lock `conn.PrepareContext(ctx, query)`, now db.numOpen == db.maxOpen , wait for release（等待g1协程释放锁）.
	// 3. g1 tx exec insert（执行插入）, wait for unlock `conn.PrepareContext(ctx, query)` to finish tx and release.
	// 调用 *sql.DB 的 prepareContext 方法，创建真正的 stmt(*sql.Stmt)
	stmt, err := conn.PrepareContext(ctx, query)
	if err != nil { // 如果创建失败
		cacheStmt.prepareErr = err
		db.Mux.Lock()
		delete(db.Stmts, query) // 加写锁，删除创建失败的stmt
		db.Mux.Unlock()
		return Stmt{}, err
	}

	// 真正的stmt创建成功
	db.Mux.Lock()
	cacheStmt.Stmt = stmt // 加锁，完成真正的复制
	db.Mux.Unlock()

	return cacheStmt, nil
}

func (db *PreparedStmtDB) BeginTx(ctx context.Context, opt *sql.TxOptions) (ConnPool, error) {
	if beginner, ok := db.ConnPool.(TxBeginner); ok {
		tx, err := beginner.BeginTx(ctx, opt)
		return &PreparedStmtTX{PreparedStmtDB: db, Tx: tx}, err
	}

	beginner, ok := db.ConnPool.(ConnPoolBeginner)
	if !ok {
		return nil, ErrInvalidTransaction
	}

	connPool, err := beginner.BeginTx(ctx, opt)
	if err != nil {
		return nil, err
	}
	if tx, ok := connPool.(Tx); ok {
		return &PreparedStmtTX{PreparedStmtDB: db, Tx: tx}, nil
	}
	return nil, ErrInvalidTransaction
}

func (db *PreparedStmtDB) ExecContext(ctx context.Context, query string, args ...interface{}) (result sql.Result, err error) {
	stmt, err := db.prepare(ctx, db.ConnPool, false, query)
	if err == nil {
		result, err = stmt.ExecContext(ctx, args...)
		if errors.Is(err, driver.ErrBadConn) {
			db.Mux.Lock()
			defer db.Mux.Unlock()
			go stmt.Close()
			delete(db.Stmts, query)
		}
	}
	return result, err
}

func (db *PreparedStmtDB) QueryContext(ctx context.Context, query string, args ...interface{}) (rows *sql.Rows, err error) {
	stmt, err := db.prepare(ctx, db.ConnPool, false, query)
	if err == nil {
		rows, err = stmt.QueryContext(ctx, args...)
		if errors.Is(err, driver.ErrBadConn) {
			db.Mux.Lock()
			defer db.Mux.Unlock()

			go stmt.Close()
			delete(db.Stmts, query)
		}
	}
	return rows, err
}

func (db *PreparedStmtDB) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	stmt, err := db.prepare(ctx, db.ConnPool, false, query)
	if err == nil {
		return stmt.QueryRowContext(ctx, args...)
	}
	return &sql.Row{}
}

func (db *PreparedStmtDB) Ping() error {
	conn, err := db.GetDBConn()
	if err != nil {
		return err
	}
	return conn.Ping()
}

type PreparedStmtTX struct {
	Tx
	PreparedStmtDB *PreparedStmtDB
}

func (db *PreparedStmtTX) GetDBConn() (*sql.DB, error) {
	return db.PreparedStmtDB.GetDBConn()
}

func (tx *PreparedStmtTX) Commit() error {
	if tx.Tx != nil && !reflect.ValueOf(tx.Tx).IsNil() {
		return tx.Tx.Commit()
	}
	return ErrInvalidTransaction
}

func (tx *PreparedStmtTX) Rollback() error {
	if tx.Tx != nil && !reflect.ValueOf(tx.Tx).IsNil() {
		return tx.Tx.Rollback()
	}
	return ErrInvalidTransaction
}

func (tx *PreparedStmtTX) ExecContext(ctx context.Context, query string, args ...interface{}) (result sql.Result, err error) {
	stmt, err := tx.PreparedStmtDB.prepare(ctx, tx.Tx, true, query)
	if err == nil {
		result, err = tx.Tx.StmtContext(ctx, stmt.Stmt).ExecContext(ctx, args...)
		if errors.Is(err, driver.ErrBadConn) {
			tx.PreparedStmtDB.Mux.Lock()
			defer tx.PreparedStmtDB.Mux.Unlock()

			go stmt.Close()
			delete(tx.PreparedStmtDB.Stmts, query)
		}
	}
	return result, err
}

func (tx *PreparedStmtTX) QueryContext(ctx context.Context, query string, args ...interface{}) (rows *sql.Rows, err error) {
	stmt, err := tx.PreparedStmtDB.prepare(ctx, tx.Tx, true, query)
	if err == nil {
		rows, err = tx.Tx.StmtContext(ctx, stmt.Stmt).QueryContext(ctx, args...)
		if errors.Is(err, driver.ErrBadConn) {
			tx.PreparedStmtDB.Mux.Lock()
			defer tx.PreparedStmtDB.Mux.Unlock()

			go stmt.Close()
			delete(tx.PreparedStmtDB.Stmts, query)
		}
	}
	return rows, err
}

func (tx *PreparedStmtTX) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	stmt, err := tx.PreparedStmtDB.prepare(ctx, tx.Tx, true, query)
	if err == nil {
		return tx.Tx.StmtContext(ctx, stmt.Stmt).QueryRowContext(ctx, args...)
	}
	return &sql.Row{}
}

func (tx *PreparedStmtTX) Ping() error {
	conn, err := tx.GetDBConn()
	if err != nil {
		return err
	}
	return conn.Ping()
}
