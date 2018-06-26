package hook

import (
	"github.com/sirupsen/logrus"
	"github.com/mintance/go-clickhouse"
	"time"
	"sync"
	"os"
	"net/url"
)

var log = logrus.New()

func init() {
	log.Formatter = &logrus.TextFormatter{
		DisableTimestamp: true,
		DisableSorting:   true,
		QuoteEmptyFields: true,
	}
	log.SetLevel(logrus.ErrorLevel)
	log.Out = os.Stderr
}

var BufferSize = 32768
var TickerPeriod = 10 * time.Second

type ClickHouse struct {
	Db      string
	Table   string
	Host    string
	Port    string
	Columns []string
	Credentials struct {
		User     string
		Password string
	}
}

type Hook struct {
	ClickHouse    *ClickHouse
	connection    *clickhouse.Conn
	levels        []logrus.Level
}

type AsyncHook struct {
	*Hook
	bus     chan map[string]interface{}
	flush   chan bool
	halt    chan bool
	flushWg *sync.WaitGroup
	Ticker  *time.Ticker
}

func (hook *Hook) Save(field map[string]interface{}) error {
	rows := buildRows(hook.ClickHouse.Columns, []map[string]interface{}{field})

	err := persist(hook.ClickHouse, hook.connection, rows)

	return err
}

func (hook *AsyncHook) SaveBatch(fields []map[string]interface{}) error {
	rows := buildRows(hook.ClickHouse.Columns, fields)
	err := persist(hook.ClickHouse, hook.connection, rows)

	return err
}

func persist(config *ClickHouse, connection *clickhouse.Conn, rows clickhouse.Rows) error {
	if rows == nil || len(rows) == 0 {
		return nil
	}

	query, err := clickhouse.BuildMultiInsert(
		config.Db+"."+config.Table,
		config.Columns,
		rows,
	)

	if err != nil {
		return err
	}

	log.Debug("Exec query")

	return query.Exec(connection)
}

func buildRows(columns []string, fields []map[string]interface{}) (rows clickhouse.Rows) {
	for _, field := range fields {
		row := clickhouse.Row{}

		for _, column := range columns {
			if _, ok := field[column]; !ok {
				field[column] = ""
			}

			row = append(row, field[column])
		}

		rows = append(rows, row)
	}

	return
}

func getStorage(config *ClickHouse) (*clickhouse.Conn, error) {

	httpTransport := clickhouse.NewHttpTransport()
	conn := clickhouse.NewConn(config.Host+":"+config.Port, httpTransport)

	params := url.Values{}
	params.Add("user", config.Credentials.User)
	params.Add("password", config.Credentials.Password)
	//conn.SetParams(params)

	if err := conn.Ping(); err != nil {
		return nil, err
	}

	return conn, nil
}

func NewHook(clickHouse *ClickHouse) (*Hook, error) {
	storage, err := getStorage(clickHouse)

	if err != nil {
		return nil, err
	}


	hook := &Hook{
		ClickHouse: clickHouse,
		connection: storage,
		levels:     nil,
	}

	return hook, nil
}

func NewAsyncHook(clickHouse *ClickHouse) (*AsyncHook, error) {
	storage, err := getStorage(clickHouse)

	if err != nil {
		return nil, err
	}

	var wg sync.WaitGroup

	hook := &AsyncHook{
		Hook: &Hook{
			ClickHouse: clickHouse,
			connection: storage,
		},
		bus:     make(chan map[string]interface{}, BufferSize),
		flush:   make(chan bool),
		halt:    make(chan bool),
		flushWg: &wg,
		Ticker:  time.NewTicker(TickerPeriod),
	}

	go hook.fire()

	return hook, nil
}

func (hook *Hook) Fire(entry *logrus.Entry) error {
	// Fill up template entry
	entry.Data = entry.Data

	if _, ok := entry.Data["msg"]; !ok {
		entry.Data["msg"] = entry.Message
	}

	entry.Data["time"] = entry.Time.Format("2006-01-02 15:04:05")
	entry.Data["level"] = entry.Level.String()

	return hook.Save(entry.Data)
}

func (hook *AsyncHook) Fire(entry *logrus.Entry) error {
	entry.Data = entry.Data

	if _, ok := entry.Data["msg"]; !ok {
		entry.Data["msg"] = entry.Message
	}

	entry.Data["time"] = entry.Time.Format("2006-01-02 15:04:05")
	entry.Data["level"] = entry.Level.String()

	hook.bus <- entry.Data

	return nil
}

func (hook *Hook) SetLevels(lvs []logrus.Level) {
	hook.levels = lvs
}

func (hook *AsyncHook) SetLevels(lvs []logrus.Level) {
	hook.levels = lvs
}

func (hook *Hook) Levels() []logrus.Level {

	if hook.levels == nil {
		return []logrus.Level{
			logrus.PanicLevel,
			logrus.FatalLevel,
			logrus.ErrorLevel,
			logrus.WarnLevel,
			logrus.InfoLevel,
			logrus.DebugLevel,
		}
	}

	return hook.levels
}

func (hook *AsyncHook) Levels() []logrus.Level {

	if hook.levels == nil {
		return []logrus.Level{
			logrus.PanicLevel,
			logrus.FatalLevel,
			logrus.ErrorLevel,
			logrus.WarnLevel,
			logrus.InfoLevel,
			logrus.DebugLevel,
		}
	}

	return hook.levels
}

func (hook *AsyncHook) Flush() {
	log.Debug("Flush...")
	hook.flushWg.Add(1)
	hook.flush <- true
	hook.flushWg.Wait()
}

func (hook *AsyncHook) Close() {
	log.Debug("Close...")
	hook.halt <- true
}

func (hook *AsyncHook) fire() {
	var buffer []map[string]interface{}

	defer hook.SaveBatch(buffer)

	for {
		select {
		case fields := <-hook.bus:
			log.Debug("Push message into bus...")
			buffer = append(buffer, fields)
			if len(buffer) >= BufferSize {
				err := hook.SaveBatch(buffer)
				if err != nil {
					log.Error(err)
				}
				buffer = buffer[:0]
			}
			continue
		default:
		}
		select {
		case fields := <-hook.bus:
			log.Debug("Push message into bus...")
			buffer = append(buffer, fields)
			if len(buffer) >= BufferSize {
				err := hook.SaveBatch(buffer)
				if err != nil {
					log.Error(err)
				}
				buffer = buffer[:0]
			}
		case <-hook.Ticker.C:
			log.Debug("Flush by ticker...")
			err := hook.SaveBatch(buffer)
			if err != nil {
				log.Error(err)
			}
			buffer = buffer[:0]
		case <-hook.flush:
			log.Debug("Flush by flush...")
			err := hook.SaveBatch(buffer)
			if err != nil {
				log.Error(err)
			}
			buffer = buffer[:0]
			hook.flushWg.Done()
		case <-hook.halt:
			log.Debug("Halt...")
			hook.Flush()
			return
		}

	}
}
