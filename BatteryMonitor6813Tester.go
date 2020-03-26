package main

import (
	"BatteryMonitor6813Tester/LTC6813"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	_ "github.com/go-sql-driver/mysql"
	"github.com/gorilla/mux"
	"io/ioutil"
	"log"
	"log/syslog"
	"net/http"
	"os"
	"periph.io/x/periph/conn/physic"
	"periph.io/x/periph/conn/pin"
	"periph.io/x/periph/conn/pin/pinreg"
	"periph.io/x/periph/conn/spi"
	"periph.io/x/periph/conn/spi/spireg"
	"periph.io/x/periph/host"
	"strconv"
	"sync"
	"time"
)

const SPI_BAUD_RATE = physic.MegaHertz * 1
const SPI_BITS_PER_WORD = 8

var ltc *LTC6813.LTC6813
var spiConnection spi.Conn
var verbose *bool
var spiDevice *string
var nErrors int
var pDB *sql.DB
var pDatabaseLogin, pDatabasePassword, pDatabaseServer, pDatabasePort, pDatabaseName *string
var ltc_lock sync.Mutex
var nDevices int

func printPin(fn string, p pin.Pin) {
	name, pos := pinreg.Position(p)
	if name != "" {
		fmt.Printf("  %-4s: %-10s found on header %s, #%d\n", fn, p, name, pos)
	} else {
		fmt.Printf("  %-4s: %-10s\n", fn, p)
	}
}

/* Set up the LTC6804 chain by repeatedly configuring and reading longer chains until a failure occurrs.
 */
func getLTC6813() (int, error) {

	var chainLength = 1

	//	for {
	//		LTC6813.New(spiConnection, 2).Initialise()
	//
	//	}

	ltc_lock.Lock()
	defer ltc_lock.Unlock()
	for {
		testLtc := LTC6813.New(spiConnection, chainLength)
		if _, err := testLtc.Test(); err != nil {
			fmt.Println(err)
			break
		}
		testLtc = nil
		chainLength++
	}
	chainLength--
	if chainLength > 0 {
		ltc = LTC6813.New(spiConnection, chainLength)
		if err := ltc.Initialise(); err != nil {
			fmt.Print(err)
			log.Fatal(err)
		}
		_, err := ltc.MeasureVoltages()
		if err != nil {
			fmt.Println("MeasureVoltages - ", err)
		}
		_, err = ltc.MeasureTemperatures()
		if err != nil {
			fmt.Println("MeasureTemperatures - ", err)
		}
	} else {
		ltc = nil
	}
	return chainLength, nil
}

func performMeasurements() {
	var total float32
	var err error
	var banks int
	fmt.Println("Measuring")
	if nDevices == 0 {
		nDevices, err = getLTC6813()
		if err != nil {
			fmt.Print(err)
			nErrors++
			return
		}
	}
	if nDevices == 0 {
		fmt.Printf("\033cNo devices found on %s - %s", *spiDevice, time.Now().Format("15:04:05.99"))
		return
	}
	banks, err = ltc.MeasureVoltagesSC()
	if err != nil {
		// Retry if it failed and ignore the failure if the retry was successful
		banks, err = ltc.MeasureVoltagesSC()
	}
	if err != nil {
		fmt.Print(" Error measuring voltages - ", err)
		time.Sleep(time.Second * 2)
		nDevices = 0
		nErrors++
	} else {
		fmt.Print("\033c")
		fmt.Printf("%d LTC6813 found on %s - %s - %d errors.\n", nDevices, *spiDevice, time.Now().Format("15:04:05.99"), nErrors)
		for bank := 0; bank < banks; bank++ {
			fmt.Printf("Bank%2d", bank)
			total = 0.0
			for cell := 0; cell < 18; cell++ {
				fmt.Printf(" : %1.4f", ltc.GetVolts(bank, cell))
				total = total + ltc.GetVolts(bank, cell)
			}
			fmt.Printf(" Sum = %2.3f\n", total)
		}
		banks, err = ltc.MeasureTemperatures()
		if err != nil {
			banks, err = ltc.MeasureTemperatures()
		}
		if err != nil {
			fmt.Print(" Error measuring temperatures - ", err)
			time.Sleep(time.Second * 2)
			nDevices = 0
			nErrors++
		}
		fmt.Println("Temperatures")
		for bank := 0; bank < banks; bank++ {
			fmt.Printf("Bank %d", bank)
			for sensor := 0; sensor < 18; sensor++ {
				temperature, err := ltc.GetTemperature(bank, sensor)
				if err != nil {
					fmt.Printf(" :   %v ", err)
				} else {
					fmt.Printf(" :  %2.1f℃", temperature)
				}
			}
			fmt.Printf(" - Reference Volts = %1.4f - Sum of Cells = %2.3f\n", ltc.GetRefVolts(bank), ltc.GetSumOfCellsVolts(bank))
		}
	}
}

func mainImpl() error {
	if !*verbose {
		log.SetOutput(ioutil.Discard)
	}
	log.SetFlags(log.Lmicroseconds)
	if flag.NArg() != 0 {
		return errors.New("unexpected argument, try -help")
	}

	for {
		nDevices, err := getLTC6813()
		if err == nil && nDevices > 0 {
			break
		}
		fmt.Println("Looking for a device")
	}
	done := make(chan bool)
	ticker := time.NewTicker(time.Second)

	go func() {
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				performMeasurements()
			}
		}

	}()

	// Configure and start the WEB server

	router := mux.NewRouter().StrictSlash(true)
	router.HandleFunc("/", getValues).Methods("GET")
	router.HandleFunc("/version", getVersion).Methods("GET")
	router.HandleFunc("/i2cread", getI2Cread).Methods("GET")
	router.HandleFunc("/i2cwrite", getI2Cwrite).Methods("GET")
	router.HandleFunc("/i2creadByte", getI2CreadByte).Methods("GET")
	router.HandleFunc("/i2cVoltage", getI2CVoltage).Methods("GET")
	router.HandleFunc("/i2cCharge", getI2CCharge).Methods("GET")
	router.HandleFunc("/i2cCurrent", getI2CCurrent).Methods("GET")
	router.HandleFunc("/i2cTemp", getI2CTemp).Methods("GET")
	http.ListenAndServe(":8080", router) // Listen on port 8080
	return nil
}

func connectToDatabase() (*sql.DB, error) {
	if pDB != nil {
		_ = pDB.Close()
		pDB = nil
	}
	var sConnectionString = *pDatabaseLogin + ":" + *pDatabasePassword + "@tcp(" + *pDatabaseServer + ":" + *pDatabasePort + ")/" + *pDatabaseName

	fmt.Println("Connecting to [", sConnectionString, "]")
	db, err := sql.Open("mysql", sConnectionString)
	if err != nil {
		return nil, err
	}
	err = db.Ping()
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, err
}

func init() {
	verbose = flag.Bool("v", false, "verbose mode")
	spiDevice = flag.String("c", "/dev/spidev0.1", "SPI device from /dev")
	pDatabaseLogin = flag.String("l", "logger", "Database Login ID")
	pDatabasePassword = flag.String("p", "logger", "Database password")
	pDatabaseServer = flag.String("s", "localhost", "Database server")
	pDatabasePort = flag.String("o", "3306", "Database port")
	pDatabaseName = flag.String("d", "battery", "Name of the database")

	flag.Parse()
	logwriter, e := syslog.New(syslog.LOG_NOTICE, "Hello")
	if e == nil {
		log.SetOutput(logwriter)
	}
	// Initialise the SPI subsystem
	if _, err := host.Init(); err != nil {
		log.Fatal(err)
	}
	p, err := spireg.Open(*spiDevice)
	if err != nil {
		log.Fatal(err)
	}

	spiConnection, err = p.Connect(SPI_BAUD_RATE, spi.Mode0, SPI_BITS_PER_WORD)
	if err != nil {
		log.Fatal(err)
	}
	nErrors = 0

	// Set up the database connection
	pDB, err = connectToDatabase()
	if err != nil {
		log.Fatalf("Failed to connect to to the database - %s - Sorry, I am giving up.", err)
	}
}

/*
	WEB Service to return the version information
*/
func getVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	fmt.Fprint(w, `<html>
  <head>
    <Cedar Technology Battery Manager>
  </head>
  <body>
    <h1>Cedar Technology Battery Manager</h1>
    <h2>Version 1.0 - January 10th 2020</h2>
  </body>
</html>`)
}

/*
WEB service to return current process values
*/

func getValues(w http.ResponseWriter, _ *http.Request) {
	ltc_lock.Lock()
	defer ltc_lock.Unlock()
	// This header allows the output to be used in a WEB page from another server as a data source for some controls
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if ltc != nil {
		_, _ = fmt.Fprintf(w, `{%s,%s}`, ltc.GetVoltagesAsJSON(), ltc.GetTemperaturesAsJSON())
	} else {
		_, _ = fmt.Fprint(w, `{"error":"No Devices"}`)
	}
}

/**
WEB service to read the I2C port
*/
func getI2Cread(w http.ResponseWriter, r *http.Request) {
	var reg int64
	sReg := r.URL.Query().Get("reg")
	if sReg != "" {
		reg, _ = strconv.ParseInt(sReg, 0, 8)
	} else {
		reg = 0x1a
	}
	sensor, _ := strconv.ParseInt(r.URL.Query().Get("sensor"), 0, 8)
	s, err := ltc.ReadI2CWord(int(sensor), LTC6813.LTC2944Address, uint8(reg))
	w.Header().Set("Access-Control-Allow-Origin", "*")
	fmt.Fprint(w, "Request = ", r.URL.Query().Get("reg"), "\n")
	if err != nil {
		fmt.Fprint(w, "Error - ", err)
	} else {
		fmt.Fprintf(w, s)
	}
}

/**
WEB service to read one 8 bit register from the I2C port
*/
func getI2CreadByte(w http.ResponseWriter, r *http.Request) {
	sensor, _ := strconv.ParseInt(r.URL.Query().Get("sensor"), 0, 8)
	var reg int64
	sReg := r.URL.Query().Get("reg")
	if sReg != "" {
		reg, _ = strconv.ParseInt(sReg, 0, 8)
	} else {
		reg = 0x1a
	}
	s, err := ltc.ReadI2CByte(int(sensor), LTC6813.LTC2944Address, uint8(reg))
	w.Header().Set("Access-Control-Allow-Origin", "*")
	fmt.Fprint(w, "Request = ", r.URL.Query().Get("reg"), "\n")
	if err != nil {
		fmt.Fprint(w, "Error - ", err)
	} else {
		fmt.Fprintf(w, s)
	}
}

/**
WEB service to read the current from the I2C port
*/
func getI2CCurrent(w http.ResponseWriter, r *http.Request) {
	sensor, _ := strconv.ParseInt(r.URL.Query().Get("sensor"), 0, 8)

	t, err := ltc.GetI2CCurrent(int(sensor))
	if err != nil {
		fmt.Fprint(w, err)
	} else {
		fmt.Fprintf(w, "Current on sensor %d = %f", sensor, t)
	}
}

/**
WEB service to read the voltage from the I2C port
*/
func getI2CVoltage(w http.ResponseWriter, r *http.Request) {
	sensor, _ := strconv.ParseInt(r.URL.Query().Get("sensor"), 0, 8)

	t, err := ltc.GetI2CVoltage(int(sensor))
	if err != nil {
		fmt.Fprint(w, err)
	} else {
		fmt.Fprintf(w, "Voltage on sensor %d = %f", sensor, t)
	}
}

/**
WEB service to read the current from the I2C port
*/
func getI2CCharge(w http.ResponseWriter, r *http.Request) {
	sensor, _ := strconv.ParseInt(r.URL.Query().Get("sensor"), 0, 8)

	t, err := ltc.GetI2CAccumulatedCharge(int(sensor))
	if err != nil {
		fmt.Fprint(w, err)
	} else {
		fmt.Fprintf(w, "Accumulated charge on sensor %d = %f", sensor, t)
	}
}

/**
WEB service to read the temperature from the I2C port
*/
func getI2CTemp(w http.ResponseWriter, r *http.Request) {
	sensor, _ := strconv.ParseInt(r.URL.Query().Get("sensor"), 0, 8)

	t, err := ltc.GetI2CTemp(int(sensor))
	if err != nil {
		fmt.Fprint(w, err)
	} else {
		fmt.Fprintf(w, "Temperature on sensor %d = %f", sensor, t)
	}
}

/**
WEB service to write to one of the I2C registers
*/
func getI2Cwrite(w http.ResponseWriter, r *http.Request) {
	var reg, value int64
	s := r.URL.Query().Get("reg")
	if s != "" {
		reg, _ = strconv.ParseInt(s, 0, 16)
	} else {
		reg = 0x1a
	}
	s = r.URL.Query().Get("value")
	if s != "" {
		value, _ = strconv.ParseInt(s, 0, 16)
	} else {
		value = 0
	}
	_, err := ltc.WriteI2CByte(2, LTC6813.LTC2944Address, uint8(reg), uint8(value))
	w.Header().Set("Access-Control-Allow-Origin", "*")
	//	fmt.Fprint(w, "Request = ", r.URL.Query().Get("reg"), "\n")
	if err != nil {
		fmt.Fprint(w, "Error - ", err)
	} else {
		fmt.Fprintf(w, "Register %d set 0x%x", reg, value)
	}
}

func main() {

	if err := mainImpl(); err != nil {
		fmt.Fprintf(os.Stderr, "Hello Error: %s.\n", err)
		os.Exit(1)
	}
	fmt.Println("Program has ended.")
}
