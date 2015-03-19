package fs

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"syscall"

	"github.com/oursky/ourd/oddb"
)

const userDBKey = "_user"

const publicDBKey = "_public"
const privateDBKey = "_private"

var dbHookFuncs []oddb.DBHookFunc

// fileConn implements oddb.Conn interface
type fileConn struct {
	Dir      string
	AppName  string
	userDB   userDatabase
	deviceDB *deviceDatabase
	publicDB oddb.Database
}

// Open returns a new connection to fs implementation
func Open(appName, dir string) (oddb.Conn, error) {
	containerPath := filepath.Join(dir, appName)
	userDBPath := filepath.Join(containerPath, userDBKey)
	deviceDBPath := filepath.Join(containerPath, "_device")
	publicDBPath := filepath.Join(containerPath, publicDBKey)

	conn := &fileConn{
		Dir:      containerPath,
		AppName:  appName,
		userDB:   newUserDatabase(userDBPath),
		deviceDB: newDeviceDatabase(deviceDBPath),
		publicDB: newDatabase(publicDBPath, publicDBKey),
	}

	return conn, nil
}

func (conn *fileConn) Close() error {
	return nil
}

func (conn *fileConn) CreateUser(info *oddb.UserInfo) error {
	return conn.userDB.Create(info)
}

func (conn *fileConn) GetUser(id string, info *oddb.UserInfo) error {
	return conn.userDB.Get(id, info)
}

func (conn *fileConn) UpdateUser(info *oddb.UserInfo) error {
	return conn.userDB.Update(info)
}

func (conn *fileConn) DeleteUser(id string) error {
	return conn.userDB.Delete(id)
}

func (conn *fileConn) GetDevice(id string, device *oddb.Device) error {
	return conn.deviceDB.Get(id, device)
}

func (conn *fileConn) SaveDevice(device *oddb.Device) error {
	return conn.deviceDB.Save(device)
}

func (conn *fileConn) DeleteDevice(id string) error {
	return conn.deviceDB.Delete(id)
}

func (conn *fileConn) PublicDB() oddb.Database {
	return conn.publicDB
}

func (conn *fileConn) PrivateDB(userKey string) oddb.Database {
	dbPath := filepath.Join(conn.Dir, userKey)
	return newDatabase(dbPath, privateDBKey)
}

func (conn *fileConn) AddDBRecordHook(hookFunc oddb.DBHookFunc) {
	dbHookFuncs = append(dbHookFuncs, hookFunc)
}

type fileDatabase struct {
	Dir       string
	Key       string
	subscriDB subscriptionDB
}

func newDatabase(dir string, key string) *fileDatabase {
	return &fileDatabase{
		Dir:       dir,
		Key:       key,
		subscriDB: newSubscriptionDB(filepath.Join(dir, "_subscription")),
	}
}

// convenient method to execute hooks if err is nil
func (db fileDatabase) executeHook(record *oddb.Record, event oddb.RecordHookEvent, err error) error {
	if err != nil {
		return err
	}

	for _, hookFunc := range dbHookFuncs {
		go hookFunc(db, record, event)
	}

	return nil
}

func (db fileDatabase) ID() string {
	return db.Key
}

func (db fileDatabase) Get(key string, record *oddb.Record) error {
	file, err := os.Open(filepath.Join(db.Dir, key))
	if err != nil {
		if os.IsNotExist(err) {
			return oddb.ErrRecordNotFound
		}
		return err
	}

	jsonDecoder := json.NewDecoder(file)
	return jsonDecoder.Decode(record)
}

func (db fileDatabase) Save(record *oddb.Record) error {
	filePath := db.recordPath(record)
	if err := os.MkdirAll(db.Dir, 0755); err != nil {
		return err
	}

	event := recordEventByPath(filePath)

	file, err := os.Create(filePath)
	if err != nil {
		return err
	}

	jsonEncoder := json.NewEncoder(file)
	err = jsonEncoder.Encode(record)

	return db.executeHook(record, event, err)
}

func (db fileDatabase) Delete(key string) error {
	record := oddb.Record{}
	if err := db.Get(key, &record); err != nil {
		return err
	}

	err := os.Remove(filepath.Join(db.Dir, key))
	return db.executeHook(&record, oddb.RecordDeleted, err)
}

type recordSorter struct {
	records []oddb.Record
	by      func(r1, r2 *oddb.Record) bool
}

func (s *recordSorter) Len() int {
	return len(s.records)
}

func (s *recordSorter) Swap(i, j int) {
	s.records[i], s.records[j] = s.records[j], s.records[i]
}

func (s *recordSorter) Less(i, j int) bool {
	less := s.by(&s.records[i], &s.records[j])
	// log.Printf("%v < %v => %v", s.records[i], s.records[j], less)
	return less
	// return s.by(&s.records[i], &s.records[j])
}

func (s *recordSorter) Sort() {
	sort.Sort(s)
}

func newRecordSorter(records []oddb.Record, sortinfo oddb.Sort) *recordSorter {
	var by func(r1, r2 *oddb.Record) bool

	field := sortinfo.KeyPath

	switch sortinfo.Order {
	default:
		by = func(r1, r2 *oddb.Record) bool {
			return reflectLess(r1.Get(field), r2.Get(field))
		}
	case oddb.Desc:
		by = func(r1, r2 *oddb.Record) bool {
			return !reflectLess(r1.Get(field), r2.Get(field))
		}
	}

	return &recordSorter{
		records: records,
		by:      by,
	}
}

// reflectLess determines whether i1 should have order less than i2.
// This func doesn't deal with pointers
func reflectLess(i1, i2 interface{}) bool {
	if i1 == nil && i2 == nil {
		return true
	}
	if i1 == nil {
		return true
	}
	if i2 == nil {
		return false
	}

	v1 := reflect.ValueOf(i1)
	v2 := reflect.ValueOf(i2)

	if v1.Kind() != v2.Kind() {
		return fmt.Sprint(i1) < fmt.Sprint(i2)
	}

	switch v1.Kind() {
	case reflect.Bool:
		b1, b2 := i1.(bool), i2.(bool)
		if b1 && !b2 { // treating bool as number, then only [1, 0] returns false
			return false
		}
		return true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v1.Int() < v2.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v1.Uint() < v2.Uint()
	case reflect.Float32, reflect.Float64:
		return v1.Float() < v2.Float()
	case reflect.String:
		return v1.String() < v2.String()
	default:
		return fmt.Sprint(i1) < fmt.Sprint(i2)
	}
}

// Query performs a query on the current Database.
//
// FIXME: Curent implementation is not complete. It assumes the first
// argument being the type of Record and always returns a Rows that
// iterates over all records of that type.
func (db fileDatabase) Query(query *oddb.Query) (*oddb.Rows, error) {
	const grepFmt = "grep -he \"{\\\"_type\\\":\\\"%v\\\"\" %v"

	if err := os.MkdirAll(db.Dir, 0755); err != nil {
		return oddb.NewRows(&memoryRows{0, []oddb.Record{}}), err
	}
	grep := fmt.Sprintf(grepFmt, query.Type, filepath.Join(db.Dir, "*"))

	var outbuf bytes.Buffer
	var errbuf bytes.Buffer

	cmd := exec.Command("sh", "-c", grep)
	cmd.Stdout = &outbuf
	cmd.Stdin = &errbuf

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			// NOTE: this cast is platform depedent and is only tested
			// on UNIX-like system
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				if status.ExitStatus() == 1 {
					log.Println("ExitStatus", 1)
					// grep has a exit status of 1 if it finds nothing
					// See: http://www.gnu.org/software/grep/manual/html_node/Exit-Status.html
					return oddb.NewRows(&memoryRows{0, []oddb.Record{}}), nil
				}
			}
		}
		log.Printf("Failed to grep: %v\nStderr: %v", err.Error(), errbuf.String())
		return oddb.NewRows(&memoryRows{0, []oddb.Record{}}), nil
	}

	records := []oddb.Record{}
	scanner := bufio.NewScanner(&outbuf)
	for scanner.Scan() {
		record := oddb.Record{}
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	if len(query.Sorts) > 0 {
		if len(query.Sorts) > 1 {
			return nil, errors.New("multiple sort order is not supported")
		}

		newRecordSorter(records, query.Sorts[0]).Sort()
	}

	return oddb.NewRows(&memoryRows{0, records}), nil
}

func (db fileDatabase) GetSubscription(key string, subscription *oddb.Subscription) error {
	return db.subscriDB.Get(key, subscription)
}

func (db fileDatabase) SaveSubscription(subscription *oddb.Subscription) error {
	return db.subscriDB.Save(subscription)
}

func (db fileDatabase) DeleteSubscription(key string) error {
	return db.subscriDB.Delete(key)
}

func (db fileDatabase) GetMatchingSubscription(record *oddb.Record) []oddb.Subscription {
	return db.subscriDB.GetMatchingSubscription(record)
}

func (db fileDatabase) recordPath(record *oddb.Record) string {
	return filepath.Join(db.Dir, record.Key)
}

type memoryRows struct {
	currentRowIndex int
	records         []oddb.Record
}

func (rs *memoryRows) Close() error {
	return nil
}

func (rs *memoryRows) Next(record *oddb.Record) error {
	if rs.currentRowIndex >= len(rs.records) {
		return io.EOF
	}

	*record = rs.records[rs.currentRowIndex]
	rs.currentRowIndex = rs.currentRowIndex + 1
	return nil
}

func init() {
	oddb.Register("fs", oddb.DriverFunc(Open))
}
