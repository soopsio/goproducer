package producer

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"fmt"

	"banyan_api"

	"github.com/BurntSushi/toml"
	"github.com/ngaut/log"
	"github.com/siddontang/go-mysql/canal"
	"github.com/siddontang/go-mysql/mysql"
	"github.com/siddontang/go-mysql/replication"
)

const (
	LogTypeSingle = iota
	LogTypeDay
	LogTypeHour
)

type MysqlPos struct {
	Addr string `toml:"addr"`
	Name string `toml:"bin_name"`
	Pos  uint32 `toml:"bin_pos"`
}

//Rail 定义Rail的结构
type Rail struct {
	*canal.DummyEventHandler

	c     *Config
	canal *canal.Canal

	//idChan   chan MessageID
	idChan   chan int64
	exitChan chan struct{}

	pos     *MysqlPos
	posLock sync.Mutex

	waitGroup WaitGroupWrapper
	poolSize  int

	IsRestart  bool
	ColumnsMap map[string][]string
	cluster    *banyan_api.ClusterClient
	client     *banyan_api.BanyanClient
	sqlcfg     MysqlConfig
	oldID      int64
}

//NewRail 初始化
func NewRail(c *Config, mysqlcfg MysqlConfig) (*Rail, error) {
	//日志目录确保存在
	dir := filepath.Dir(c.LogConfig.Path)
	exist, _ := PathExists(dir)

	if !exist {
		err := os.Mkdir(dir, os.ModePerm)

		if err != nil {
			return nil, err
		}
	}

	//配置日志
	log.SetHighlighting(c.LogConfig.Highlighting)
	log.SetLevel(log.StringToLogLevel(c.LogConfig.Level))
	log.SetFlags(log.Lshortfile | log.Ldate | log.Ltime)
	log.SetOutputByName(c.LogConfig.Path)

	if c.LogConfig.Type == LogTypeDay {
		log.SetRotateByDay()
	} else if c.LogConfig.Type == LogTypeHour {
		log.SetRotateByHour()
	}

	cfg := canal.NewDefaultConfig()

	cfg.Addr = mysqlcfg.Addr
	cfg.User = mysqlcfg.User
	cfg.Password = mysqlcfg.Password
	cfg.Dump.ExecutionPath = "" //不支持mysqldump
	cfg.Flavor = mysqlcfg.Flavor
	cfg.LogLevel = c.LogConfig.Level

	if canalIns, err := canal.NewCanal(cfg); err != nil {
		log.Fatal(err)
		return nil, err
	} else {
		r := &Rail{
			c:     c,
			canal: canalIns,
			//idChan:     make(chan MessageID, 4096),
			idChan:     make(chan int64, 4096),
			exitChan:   make(chan struct{}),
			IsRestart:  false,
			ColumnsMap: make(map[string][]string),
			oldID:      0,
		}

		log.Errorf("ns:%s,table:%s", mysqlcfg.NsName, mysqlcfg.TableName)
		clusters := make([]string, 0)
		for _, v := range c.ClusterConfig.Agents {
			clusters = append(clusters, v)
		}
		r.cluster = banyan_api.NewClusterClient(clusters)
		r.client, err = r.cluster.GetBanyanClient(mysqlcfg.NsName, mysqlcfg.TableName, 3000, 3)
		if err != nil {
			log.Errorf("GetBanyanClient failed: %v", err)
			return nil, err
		}
		r.sqlcfg = mysqlcfg
		//注册RowsEventHandler
		r.canal.SetEventHandler(r)

		pos, err := r.loadMasterInfo()
		if err != nil {
			log.Fatalf("load binlog position error - %s", err)
		}

		//启动canal
		r.canal.StartFrom(*pos)

		//启动msg id分配器
		r.waitGroup.Wrap(func() { r.idPump() })

		//定时保存binlog position
		r.waitGroup.Wrap(func() { r.saveMasterInfoLoop() })

		log.Info("rail start ok. id = %s", r.sqlcfg.Id)
		return r, nil
	}
}

//Close 关闭Rail,释放资源
func (r *Rail) Close() {

	//关闭canal
	r.canal.Close()

	//save binlog postion

	pos := r.canal.SyncedPosition()
	err := r.saveMasterInfo(pos.Name, pos.Pos)
	if err != nil {
		log.Warnf("save binlog position error when closing - %s", err)
	}
	//关闭topic
	//err = r.topic.Close()

	close(r.exitChan)

	r.waitGroup.Wait()

	log.Info("rail safe close. id = %s", r.sqlcfg.Id)
}

func (r *Rail) OnDDL(nextPos mysql.Position, queryEvent *replication.QueryEvent) error {
	query := string(queryEvent.Query)
	if strings.ToUpper(query) != "BEGIN" {

		log.Debugf("111pos:%d schema:%s statement: %s ", nextPos.Pos, queryEvent.Schema, query)

		query = strings.ToLower(query)
		tempStr := make([]string, 0)
		num := 0
		for _, v := range query {
			if v == 32 { //space
				num = num + 1
				if num == 2 {
					num = num - 1
					continue
				}
				tempStr = append(tempStr, string(v))
			} else {
				num = 0
				tempStr = append(tempStr, string(v))
			}
		}
		strQuery := ""
		for _, v := range tempStr {
			strQuery = strQuery + v
		}
		var id int64
		if string(queryEvent.Schema) != "" {
			log.Infof("pos:%d schema:%s statement: %s ", nextPos.Pos, queryEvent.Schema, strQuery)
			if strings.Contains(strQuery, "drop") {
				err := r.saveMasterInfo(nextPos.Name, nextPos.Pos)
				if err != nil {
					log.Warnf("save binlog position error  - %s", err)
				}
				defer r.Close()
				return errors.New("drop database need to sync binlog pos")
			}
			if strings.Contains(strQuery, "create database") {
				select {
				case id = <-r.idChan:
				}
				err := r.saveMasterInfo(nextPos.Name, nextPos.Pos)
				if err != nil {
					log.Warnf("save binlog position error - %s", err)
				}
				r.sqlProcessing(id, strQuery, "common")
			}
			if strings.Contains(strQuery, "create table") { //原操作没有指定库名，必须拼接上
				/*
					pos := -1
					pos = strings.Index(strQuery, "create table ")
					strQuery = strQuery[pos+len("create table ") : len(strQuery)]
					tempStr := fmt.Sprintf("create table %s.", queryEvent.Schema)
					tempStr = tempStr + strQuery
				*/
				select {
				case id = <-r.idChan:
				}
				err := r.saveMasterInfo(nextPos.Name, nextPos.Pos)
				if err != nil {
					log.Warnf("save binlog position error - %s", err)
				}
				r.sqlProcessing(id, strQuery, string(queryEvent.Schema))
			}
		} else {
			log.Infof("pos:%d schema is null, statement: %s", nextPos.Pos, strQuery)
			//alter table
			if strings.Contains(strQuery, "create table") {
				err := r.saveMasterInfo(nextPos.Name, nextPos.Pos)
				if err != nil {
					log.Warnf("save binlog position error  - %s", err)
				}
				defer r.Close()
				return errors.New("create table but dont have schema")
			}
			if strings.Contains(strQuery, "alter table") {
				select {
				case id = <-r.idChan:
				}
				r.ProcessAlter(queryEvent)
				r.sqlProcessing(id, strQuery, "common")
				err := r.saveMasterInfo(nextPos.Name, nextPos.Pos)
				if err != nil {
					log.Warnf("save binlog position error - %s", err)
				}

			}
		}
	} else {
		//log.Infof(" strQuery: %s", queryEvent.Query)   //BEGIN
	}

	//var err error = errors.New("this is a new error")
	return nil
}

func (r *Rail) ProcessAlter(queryEvent *replication.QueryEvent) error {
	schemaTable := "" //fmt.Sprintf("%s.%s", queryEvent.Schema, queryEvent.Table.Name)
	fields := make([]string, 0)
	query := strings.ToLower(string(queryEvent.Query))
	tempStr := make([]string, 0)
	num := 0
	for _, v := range query {
		if v == 32 { //space
			num = num + 1
			if num == 2 {
				num = num - 1
				continue
			}
			tempStr = append(tempStr, string(v))
		} else {
			num = 0
			tempStr = append(tempStr, string(v))
		}
	}
	strQuery := ""
	for _, v := range tempStr {
		strQuery = strQuery + v
	}
	log.Debugf("alter = %s ", string(strQuery))
	pos := -1
	pos = strings.Index(strQuery, "alter table ")
	strQuery = strQuery[pos+len("alter table ") : len(strQuery)]
	log.Debugf("1212 = %s ", strQuery)
	pos = strings.Index(strQuery, " ")
	schemaTable = strQuery[0:pos]
	log.Debugf("33 = %s ", schemaTable)
	columns, ok := r.ColumnsMap[schemaTable]
	//如果 ok 是 true, 则存在，否则不存在 /

	if ok {
		fields = columns
	} else {
		//return errors.New("alter table but not found schema and table")
	}
	strQuery = strQuery[pos+len(" ") : len(strQuery)]
	for {
		ischanged := false
		if strings.Contains(strQuery, "add column ") {
			pos = strings.Index(strQuery, "column ")
			strQuery = strQuery[pos+len("column ") : len(strQuery)]
			pos = strings.Index(strQuery, " ")
			columnName := strQuery[0:pos]
			fields = append(fields, strQuery[0:pos])
			strQuery = strQuery[pos+len(columnName)+1 : len(strQuery)]
			ischanged = true
		} else if strings.Contains(strQuery, "add") {
			pos = strings.Index(strQuery, "add ")
			strQuery = strQuery[pos+len("add ") : len(strQuery)]
			pos = strings.Index(strQuery, " ")
			columnName := strQuery[0:pos]
			log.Debugf("columnName = %s ", columnName)
			fields = append(fields, strQuery[0:pos])
			strQuery = strQuery[pos+len(columnName)+1 : len(strQuery)]
			log.Debugf("strQuery = %s ", strQuery)
			ischanged = true
		} else if strings.Contains(strQuery, "drop ") {
			pos = strings.Index(strQuery, "drop ")
			strQuery = strQuery[pos+len("drop ") : len(strQuery)]
			pos = strings.Index(strQuery, ",")
			columnName := strQuery[0:pos]
			log.Debugf("columnName = %s ", columnName)
			fieldsTmp := make([]string, 0)
			for _, v := range fields {
				if v == columnName {
					continue
				}
				fieldsTmp = append(fieldsTmp, v)
			}
			fields = fieldsTmp
			strQuery = strQuery[pos+1 : len(strQuery)]
			log.Debugf("123= %s ", strQuery)
			ischanged = true
		}
		if ischanged == false {
			break
		}
	}

	strQuery = strings.TrimSpace(strQuery)
	log.Debugf(" = %s  pos =%d   fields =%s", strQuery, pos, fields)
	r.ColumnsMap[schemaTable] = fields
	log.Debugf("###= %s", r.ColumnsMap)
	return nil
}

//onRow 实现接口RowEventHandler,处理binlog事件
func (r *Rail) OnRow(e *canal.RowsEvent) error {
	defer func() {
		if err := recover(); err != nil {
			log.Errorf("internal error - %s", err)
		}
	}()
	log.Debugf("Action = %s", e.Action)
	/*
		if r.c.TopicConfig.Schema != "" && e.Table.Schema != r.c.TopicConfig.Schema {
			return nil
		}

		if r.c.TopicConfig.Table != "" {
			regExp, err := regexp.Compile(r.c.TopicConfig.Table)
			//正则表达式出错
			if err != nil {
				log.Errorf("regexp(%s) error - %s", r.c.TopicConfig.Table, err)
				return err
			}
			if !regExp.Match([]byte(e.Table.Name)) {
				return nil
			}
		}
	*/
	select {
	case id := <-r.idChan:
		msg := NewMessage(id, e, &r.ColumnsMap)

		//log.Infof("push message(id=%s db=%s table=%s action=%s pk=%s) to topic", msg.ID, msg.Schema, msg.Table, msg.Action, msg.Brief())
		log.Debugf("message = %s", msg.Detail())
		var res int = 0
		if msg.Action == "insert" {
			res = r.insertSql(*msg) //保证msg只读
		} else if msg.Action == "delete" {
			res = r.deleteSql(*msg)
		} else if msg.Action == "update" {
			res = r.updateSql(*msg)
		} else {
			log.Errorf("Action unkonw!")
			return errors.New("Action unkonw!")
		}
		if res != 0 {
			log.Errorf("analyses failed!")
			return errors.New("analyses failed!")
		}
		return nil
	}
}

func typeof(v interface{}) string {
	switch t := v.(type) {
	case uint:
		return "num"
	case uint8:
		return "num"
	case uint16:
		return "num"
	case uint32:
		return "num"
	case int:
		return "num"
	case int8:
		return "num"
	case int32:
		return "num"
	case int64:
		return "num"
	case float64:
		return "float"
	case float32:
		return "float"
	case bool:
		return "num"
	case nil:
		return "null"

	case string:
		return "string"
	default:
		_ = t
		return "unknown"
	}
}

func (r *Rail) updateSql(msg Message) int {

	sql := ""
	if msg.Action == "update" {
		// db.Query("update binlog_test.usertb set user_name='binlogtest' where id in (2,3)")
		// update schema.table set name='xxxx', age=2 where id in(1,2);
		primary_keys := msg.PrimaryKeys[0][0] //初始值主键是内容需要根据内容查找主键
		pk_type := typeof(primary_keys)
		sqlStart := fmt.Sprintf("update %s.%s set ", msg.Schema, msg.Table)
		sqlMid1 := " where "
		sqlMid := " in "
		sqlEnd := ");"
		count := 0
		processedString := make(map[string]int)
		for i, v := range msg.Rows {
			sorted_keys := make([]string, 0)
			for k, _ := range v {
				sorted_keys = append(sorted_keys, k)
			}
			// sort 'string' key in increasing order
			sort.Strings(sorted_keys)
			for _, k := range sorted_keys {
				if msg.RawRows[i][k] == primary_keys && typeof(msg.RawRows[i][k]) == pk_type {
					primary_keys = k
					continue //主键修不修改全部忽略
				}
				if msg.RawRows[i][k] == v[k] && typeof(v[k]) == typeof(msg.RawRows[i][k]) {
					continue
				} else {
					_, ok := processedString[k]
					if ok {
						continue
					}
					if typeof(v[k]) == "string" {
						if count == 0 {
							sqlStart = sqlStart + fmt.Sprintf("%s = '%s'", k, v[k])
						} else {
							sqlStart = sqlStart + fmt.Sprintf(",%s = '%s'", k, v[k])
						}
						count++
						processedString[k] = 1
					} else if typeof(v[k]) == "num" {
						if count == 0 {
							sqlStart = sqlStart + fmt.Sprintf("%s = %d", k, v[k])
						} else {
							sqlStart = sqlStart + fmt.Sprintf(",%s = %d", k, v[k])
						}
						count++
						processedString[k] = 1
					} else if typeof(v[k]) == "null" {
						log.Debugf("k1 = %s  v1 = '%s' type= %s", k, v[k], typeof(v[k]))
						if count == 0 {
							sqlStart = sqlStart + fmt.Sprintf("%s = null", k)
						} else {
							sqlStart = sqlStart + fmt.Sprintf(",%s = null", k)
						}
						count++
						processedString[k] = 1
					} else if typeof(v[k]) == "float" {
						log.Debugf("k1 = %s  v1 = %f type= %s", k, v[k], typeof(v[k]))
						if count == 0 {
							sqlStart = sqlStart + fmt.Sprintf("%s = %f", k, v[k])
						} else {
							sqlStart = sqlStart + fmt.Sprintf(",%s = %f", k, v[k])
						}
						count++
						processedString[k] = 1
					}
				}
			}
		}

		pk_type_after := typeof(primary_keys)
		if pk_type_after == "num" {
			sqlMid1 = sqlMid1 + fmt.Sprintf("%d ", primary_keys)
		} else if pk_type_after == "string" {
			sqlMid1 = sqlMid1 + fmt.Sprintf("%s ", primary_keys)
		}

		for i, v1 := range msg.PrimaryKeys {
			if i%2 != 0 { //去重
				continue
			}
			for _, v2 := range v1 {
				if typeof(v2) == "string" {
					if i == 0 {
						sqlMid = sqlMid + fmt.Sprintf("('%s'", v2)
					} else {
						sqlMid = sqlMid + fmt.Sprintf(",'%s'", v2)
					}
				} else if typeof(v2) == "num" {
					if i == 0 {
						sqlMid = sqlMid + fmt.Sprintf("(%d", v2)
					} else {
						sqlMid = sqlMid + fmt.Sprintf(",%d", v2)
					}
				}
			}
		}

		sql = sqlStart + sqlMid1 + sqlMid + sqlEnd
		log.Debugf("update sql: %s", sql)
	} else {
		log.Errorf("not update sql")
		return -1
	}

	r.sqlProcessing(msg.ID, sql, msg.Schema)
	return 0
}

func (r *Rail) deleteSql(msg Message) int {
	var sql string = ""
	if msg.Action == "delete" {
		sqlStart := fmt.Sprintf("delete from %s.%s where ", msg.Schema, msg.Table)
		primary_keys := msg.PrimaryKeys[0][0] //初始值主键是内容需要根据内容查找主键
		pk_type := typeof(primary_keys)
		sqlMid := "in "
		sqlEnd := ");"
		for _, v := range msg.Rows {
			sorted_keys := make([]string, 0)
			for k, _ := range v {
				sorted_keys = append(sorted_keys, k)
			}
			// sort 'string' key in increasing order
			sort.Strings(sorted_keys)
			for _, k := range sorted_keys {
				if v[k] == primary_keys && typeof(v[k]) == pk_type {
					primary_keys = k
				}
			}
		}

		pk_type_after := typeof(primary_keys)
		if pk_type_after == "num" {
			sqlStart = sqlStart + fmt.Sprintf("%d ", primary_keys)
		} else if pk_type_after == "string" {
			sqlStart = sqlStart + fmt.Sprintf("%s ", primary_keys)
		}

		for i, v1 := range msg.PrimaryKeys {
			for _, v2 := range v1 {
				if typeof(v2) == "string" {
					if i == 0 {
						sqlMid = sqlMid + fmt.Sprintf("('%s'", v2)
					} else {
						sqlMid = sqlMid + fmt.Sprintf(",'%s'", v2)
					}
				} else if typeof(v2) == "num" {
					if i == 0 {
						sqlMid = sqlMid + fmt.Sprintf("(%d", v2)
					} else {
						sqlMid = sqlMid + fmt.Sprintf(",%d", v2)
					}
				} else if typeof(v2) == "null" {
					if i == 0 {
						sqlMid = sqlMid + fmt.Sprintf("(null")
					} else {
						sqlMid = sqlMid + fmt.Sprintf(",null")
					}
				}
			}
		}
		sql = sqlStart + sqlMid + sqlEnd
		log.Debugf("delete sql %s", sql)
	} else {
		log.Errorf("not delete sql")
		return -1
	}

	r.sqlProcessing(msg.ID, sql, msg.Schema)
	return 0
}

func (r *Rail) insertSql(msg Message) int {

	var sql string = ""
	//insert into schema.table(id,name) values(1,'banli');
	sqlStart := fmt.Sprintf("insert into %s.%s(", msg.Schema, msg.Table)
	sqlMid := ") values"
	sqlEnd := ";"
	tmp_keys := make([]string, 0)
	if msg.Action == "insert" {
		for j, v := range msg.Rows {
			sorted_keys := make([]string, 0)
			for k, _ := range v {
				sorted_keys = append(sorted_keys, k)
			}

			// sort 'string' key in increasing order
			sort.Strings(sorted_keys)
			tmp_keys = sorted_keys
			for i, k := range sorted_keys {
				if typeof(v[k]) == "string" {
					log.Debugf("k1 = %s  v1 = '%s' type= %s", k, v[k], typeof(v[k]))
					if i == 0 {
						sqlMid = sqlMid + fmt.Sprintf("('%s'", v[k])
					} else {
						sqlMid = sqlMid + fmt.Sprintf(",'%s'", v[k])
					}
				} else if typeof(v[k]) == "num" {
					log.Debugf("k1 = %s  v1 = %d type= %s", k, v[k], typeof(v[k]))
					if i == 0 {
						sqlMid = sqlMid + fmt.Sprintf("(%d", v[k])
					} else {
						sqlMid = sqlMid + fmt.Sprintf(",%d", v[k])
					}
				} else if typeof(v[k]) == "null" {
					log.Debugf("k1 = %s  v1 = '%s' type= %s", k, v[k], typeof(v[k]))
					if i == 0 {
						sqlMid = sqlMid + fmt.Sprintf("(null")
					} else {
						sqlMid = sqlMid + fmt.Sprintf(",null")
					}
				} else if typeof(v[k]) == "float" {
					log.Debugf("k1 = %s  v1 = %f type= %s", k, v[k], typeof(v[k]))
					if i == 0 {
						sqlMid = sqlMid + fmt.Sprintf("(%f", v[k])
					} else {
						sqlMid = sqlMid + fmt.Sprintf(",%f", v[k])
					}
				}
			}
			if j == (len(msg.Rows) - 1) {
				sqlMid = sqlMid + fmt.Sprintf(")")
			} else {
				sqlMid = sqlMid + fmt.Sprintf("),")
			}
			log.Debugf("============= ")
		}

		for i, k := range tmp_keys {
			if i == 0 {
				sqlStart = sqlStart + k
			} else {
				sqlStart = sqlStart + fmt.Sprintf(",%s", k)
			}
		}
		sql = sqlStart + sqlMid + sqlEnd
		log.Debugf("sqlQuary = %s", sql)
	} else {
		log.Errorf("not insert sql")
		return -1
	}

	r.sqlProcessing(msg.ID, sql, msg.Schema)
	return 0
}

func (r *Rail) OnRotate(e *replication.RotateEvent) error {
	return r.saveMasterInfo(string(e.NextLogName), uint32(e.Position))
}

//String  实现接口RowEventHandler
func (r *Rail) String() string {
	return "rail"
}

func (r *Rail) getMasterInfoPath() string {
	return r.sqlcfg.DataPath //+ "/" + "master.info"
}

func (r *Rail) loadMasterInfo() (*mysql.Position, error) {
	f, err := os.Open(r.getMasterInfoPath())
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	} else if os.IsNotExist(err) {
		//文件不存在,默认从最新的位置开始
		return r.getNewestPos()
	}

	defer f.Close()

	var mysqlPos MysqlPos
	_, err = toml.DecodeReader(f, &mysqlPos)
	if err != nil || mysqlPos.Addr != r.sqlcfg.Addr || mysqlPos.Name == "" {
		return r.getNewestPos()
	}

	return &mysql.Position{mysqlPos.Name, mysqlPos.Pos}, nil
}

//得到最新的binlog位置
func (r *Rail) getNewestPos() (*mysql.Position, error) {
	result, err := r.canal.Execute("SHOW MASTER STATUS")
	if err != nil {
		return nil, fmt.Errorf("show master status error - %s", err)
	}

	if result.Resultset.RowNumber() != 1 {
		return nil, errors.New("select master info error")
	}

	binlogName, _ := result.GetStringByName(0, "File")
	binlogPos, _ := result.GetIntByName(0, "Position")

	log.Infof("fetch mysql(%s)'s the newest pos:(%s, %d)", r.sqlcfg.Addr, binlogName, binlogPos)

	return &mysql.Position{binlogName, uint32(binlogPos)}, nil
}

func (r *Rail) saveMasterInfo(posName string, pos uint32) error {
	r.posLock.Lock()
	defer r.posLock.Unlock()

	var buf bytes.Buffer
	e := toml.NewEncoder(&buf)

	mysqlPos, err := r.loadMasterInfo()
	if err == nil && r.pos == nil {
		r.pos = &MysqlPos{
			Addr: r.sqlcfg.Addr,
			Name: mysqlPos.Name,
			Pos:  mysqlPos.Pos,
		}
	}

	if err != nil && r.pos == nil {
		r.pos = &MysqlPos{
			Addr: r.sqlcfg.Addr,
			Name: posName,
			Pos:  pos,
		}
	}

	if r.pos.Name == posName && pos <= r.pos.Pos {
		return nil
	} else {
		r.pos.Name = posName
		r.pos.Pos = pos
	}

	/*
		if r.pos == nil {

			r.pos = &MysqlPos{
				Addr: r.sqlcfg.Addr,
				Name: posName,
				Pos:  pos,
			}

		} else {
			if r.pos.Name == posName && r.pos.Pos <= pos {
				return nil
			}
			r.pos.Name = posName
			r.pos.Pos = pos
		}
	*/
	e.Encode(r.pos)

	f, err := os.Create(r.getMasterInfoPath())
	if err != nil {
		log.Warnf("create master info file error - %s", err)
		return err
	}
	_, err = f.Write(buf.Bytes())
	if err != nil {
		log.Warnf("save master info to file  error - %s", err)
		return err
	}

	log.Debug("save binlog position succ")
	return nil
}

func (r *Rail) saveMasterInfoLoop() {
	ticker := time.NewTicker(r.c.BinlogFlushMs * time.Millisecond)
	for {
		select {
		case <-ticker.C:
			pos := r.canal.SyncedPosition()
			if r.pos == nil || pos.Name != r.pos.Name || pos.Pos != r.pos.Pos {
				err := r.saveMasterInfo(pos.Name, pos.Pos)
				if err != nil {
					log.Warnf("save binlog position error from per second - %s", err)
				}
			}

		case <-r.exitChan:
			log.Info("save binlog position loop exit.")
			return
		}
	}

}

/*
func (r *Rail) idPump() {
	factory := &guidFactory{}
	lastError := time.Unix(0, 0)
	workerID := int64(0)
	for {
		id, err := factory.NewGUID(workerID)
		if err != nil {
			now := time.Now()
			if now.Sub(lastError) > time.Second {
				// only print the error once/second
				log.Errorf("id pump error(%s)", err)
				lastError = now
			}
			runtime.Gosched()
			continue
		}
		select {
		case r.idChan <- id.Hex():
		case <-r.exitChan:
			goto exit
		}
	}

exit:
	log.Infof("ID: closing")
}
*/
func (r *Rail) idPump() {
	var id int64
	id = 0
	for {
		id++
		select {
		case r.idChan <- id:
		case <-r.exitChan:
			goto exit
		}
	}

exit:
	log.Infof("ID: closing")
}
func (r *Rail) sqlProcessing(id int64, quary string, schema string) error {
	if id <= r.oldID {
		log.Errorf("id  error qpush failed.id:%d schema:%s  sql:(%s)", id, schema, quary)
		r.Close()
	}
	_, err := r.client.Qpush(schema, quary)
	if err != nil {
		log.Errorf("qpush failed: %v", err)
		r.Close()
		return err
	}
	log.Infof("Qpush id :%d schema:%s  sql:(%s)", id, schema, quary)
	r.oldID = id
	return nil
}
