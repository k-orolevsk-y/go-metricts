package dbstorage

import (
	"context"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jmoiron/sqlx"
	"github.com/k-orolevsk-y/go-metricts-tpl/internal/server/errs"
	"github.com/k-orolevsk-y/go-metricts-tpl/internal/server/models"
	"github.com/k-orolevsk-y/go-metricts-tpl/pkg/logger"
	"net"
	"time"
)

var maximumNumberOfRetries = []int{1, 3, 5}

type (
	databaseStorage struct {
		db  *sqlx.DB
		log logger.Logger

		prepares prepares
	}

	prepares struct {
		getGaugeMetric    *sqlx.NamedStmt
		getCounterMetric  *sqlx.NamedStmt
		setOrUpdateMetric *sqlx.NamedStmt
	}
)

func New(db *sqlx.DB, log logger.Logger) (*databaseStorage, error) {
	dbStorage := &databaseStorage{
		db:  db,
		log: log,
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	if err := dbStorage.Ping(ctx); err != nil {
		dbStorage.log.Errorf("Failed to connect database to create table and prepare queries: %s", err)
		return dbStorage, nil
	}

	const schema = `CREATE TABLE IF NOT EXISTS metrics (
	    "_id" SERIAL,
		"name" TEXT NOT NULL,
		"mtype" VARCHAR(12) NOT NULL DEFAULT 'gauge',
		"delta" BIGINT NOT NULL DEFAULT 0,
		"value" DOUBLE PRECISION NOT NULL DEFAULT 0.0,
		CONSTRAINT unique_id_mtype UNIQUE (name, mtype),
		PRIMARY KEY (_id)
	)`

	if _, err := dbStorage.db.ExecContext(ctx, schema); err != nil {
		return nil, err
	} else {
		dbStorage.log.Debugf("The tables for the database were successfully created, if they not existed.")
	}

	if err := dbStorage.buildPrepares(ctx); err != nil {
		return nil, err
	} else {
		dbStorage.log.Debugf("SQL Requests are prepared.")
	}

	return dbStorage, nil
}

func (dbStorage *databaseStorage) buildPrepares(ctx context.Context) error {
	preparesData := map[string]string{
		"getGaugeMetric":   `SELECT value FROM metrics WHERE name = :name AND mtype = 'gauge'`,
		"getCounterMetric": `SELECT delta FROM metrics WHERE name = :name AND mtype = 'counter'`,
		"setOrUpdateMetric": `INSERT INTO metrics (name, mtype, delta, value) 
				VALUES (:name, :mtype, :delta, :value)
			ON CONFLICT (name, mtype) DO 
			    UPDATE SET delta = metrics.delta + excluded.delta, value = excluded.value`,
	}

	for key, sql := range preparesData {
		p, err := dbStorage.db.PrepareNamedContext(ctx, sql)
		if err != nil {
			return err
		}

		switch key {
		case "getGaugeMetric":
			dbStorage.prepares.getGaugeMetric = p
		case "getCounterMetric":
			dbStorage.prepares.getCounterMetric = p
		case "setOrUpdateMetric":
			dbStorage.prepares.setOrUpdateMetric = p
		}
	}

	return nil
}

func (dbStorage *databaseStorage) Close() error {
	var closeErrs []error

	closeErrs = append(closeErrs, dbStorage.prepares.getGaugeMetric.Close())
	closeErrs = append(closeErrs, dbStorage.prepares.getCounterMetric.Close())
	closeErrs = append(closeErrs, dbStorage.prepares.setOrUpdateMetric.Close())
	closeErrs = append(closeErrs, dbStorage.db.Close())

	return errors.Join(closeErrs...)
}

func (dbStorage *databaseStorage) NewTx() (models.StorageTx, error) {
	txDB, err := dbStorage.db.Beginx()
	if err != nil {
		return nil, err
	}

	t := &tx{
		txDB: txDB,
		log:  dbStorage.log,
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
	defer cancel()

	if err = t.buildPrepares(ctx); err != nil {
		return nil, err
	}

	dbStorage.log.Debugf("A new transaction has been successfully created.")
	return t, nil
}

func (dbStorage *databaseStorage) SetGauge(name string, value *float64) (err error) {
	for _, timeSleep := range maximumNumberOfRetries {
		_, err = dbStorage.prepares.setOrUpdateMetric.ExecContext(context.Background(), map[string]interface{}{"name": name, "mtype": "gauge", "delta": 0, "value": value})

		ok, parsedErr := parseRetriableError(err)
		if !ok {
			return
		}

		dbStorage.log.Errorf("Error set gauge metric %s: \"%s\". Retrying %ds...", name, parsedErr, timeSleep)
		time.Sleep(time.Duration(timeSleep) * time.Second)
	}
	return
}

func (dbStorage *databaseStorage) AddCounter(name string, value *int64) (err error) {
	for _, timeSleep := range maximumNumberOfRetries {
		_, err = dbStorage.prepares.setOrUpdateMetric.ExecContext(context.Background(), map[string]interface{}{"name": name, "mtype": "counter", "delta": value, "value": 0.0})

		ok, parsedErr := parseRetriableError(err)
		if !ok {
			return
		}

		dbStorage.log.Errorf("Error add counter metric %s: \"%s\". Retrying after %ds...", name, parsedErr, timeSleep)
		time.Sleep(time.Duration(timeSleep) * time.Second)
	}
	return
}

func (dbStorage *databaseStorage) GetGauge(name string) (value *float64, err error) {
	for _, timeSleep := range maximumNumberOfRetries {
		err = dbStorage.prepares.getGaugeMetric.GetContext(context.Background(), &value, map[string]interface{}{"name": name})

		ok, parsedErr := parseRetriableError(err)
		if !ok {
			return
		}

		dbStorage.log.Errorf("Error get gauge metric %s: \"%s\". Retrying after %ds...", name, parsedErr, timeSleep)
		time.Sleep(time.Duration(timeSleep) * time.Second)
	}
	return
}

func (dbStorage *databaseStorage) GetCounter(name string) (value *int64, err error) {
	for _, timeSleep := range maximumNumberOfRetries {
		err = dbStorage.prepares.getCounterMetric.GetContext(context.Background(), &value, map[string]interface{}{"name": name})

		ok, parsedErr := parseRetriableError(err)
		if !ok {
			return
		}

		dbStorage.log.Errorf("Error get counter metric %s: \"%s\". Retrying after %ds...", name, parsedErr, timeSleep)
		time.Sleep(time.Duration(timeSleep) * time.Second)
	}
	return
}

func (dbStorage *databaseStorage) GetAll() (metrics []models.MetricsValue, err error) {
	for _, timeSleep := range maximumNumberOfRetries {
		err = dbStorage.db.SelectContext(context.Background(), &metrics, "SELECT name, mtype, delta, value FROM metrics")

		ok, parsedErr := parseRetriableError(err)
		if !ok {
			return
		}

		dbStorage.log.Errorf("Error get all metrics: \"%s\". Retrying after %ds...", parsedErr, timeSleep)
		time.Sleep(time.Duration(timeSleep) * time.Second)
	}
	return
}

func (dbStorage *databaseStorage) Ping(ctx context.Context) error {
	return dbStorage.db.PingContext(ctx)
}

func (dbStorage *databaseStorage) GetMiddleware() gin.HandlerFunc {
	return func(_ *gin.Context) {}
}

func (dbStorage *databaseStorage) String() string {
	var databaseName string
	_ = dbStorage.db.Get(&databaseName, "SELECT current_database()")

	if databaseName == "" {
		databaseName = "(Error: Invalid database name)"
	}

	return fmt.Sprintf("DBStorage - %s", databaseName)
}

func parseRetriableError(err error) (bool, *errs.DatabaseError) {
	if err == nil {
		return false, nil
	}

	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true, errs.NewDatabaseError("net.OpError", opErr)
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || !pgerrcode.IsConnectionException(pgErr.Code) {
		return false, nil
	}

	return true, errs.NewDatabaseError("pgconn.PgError", pgErr)
}
