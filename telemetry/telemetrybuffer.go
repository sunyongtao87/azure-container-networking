// Copyright 2018 Microsoft. All rights reserved.
// MIT License

package telemetry

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Azure/azure-container-networking/common"
	"github.com/Azure/azure-container-networking/log"
	"github.com/Azure/azure-container-networking/platform"
)

// FdName - file descriptor name
// Delimiter - delimiter for socket reads/writes
// azureHostReportURL - host net agent url of type payload
// DefaultDncReportsSize - default DNC report slice size
// DefaultCniReportsSize - default CNI report slice size
// DefaultNpmReportsSize - default NPM report slice size
// DefaultInterval - default interval for sending payload to host
// MaxPayloadSize - max payload size (~2MB)
const (
	FdName             = "azure-vnet-telemetry"
	Delimiter          = '\n'
	azureHostReportURL = "http://169.254.169.254/machine/plugins?comp=netagent&type=payload"
	DefaultInterval    = 60 * time.Second
	logName            = "azure-vnet-telemetry"
  MaxPayloadSize     = 2097
)

var telemetryLogger = log.NewLogger(logName, log.LevelInfo, log.TargetStderr)

// TelemetryBuffer object
type TelemetryBuffer struct {
	client             net.Conn
	listener           net.Listener
	connections        []net.Conn
	azureHostReportURL string
	payload            Payload
	FdExists           bool
	Connected          bool
	data               chan interface{}
	cancel             chan bool
}

// Payload object holds the different types of reports
type Payload struct {
	DNCReports []DNCReport
	CNIReports []CNIReport
	NPMReports []NPMReport
	CNSReports []CNSReport
}

// NewTelemetryBuffer - create a new TelemetryBuffer
func NewTelemetryBuffer(hostReportURL string) *TelemetryBuffer {
	var tb TelemetryBuffer

	if hostReportURL == "" {
		tb.azureHostReportURL = azureHostReportURL
	}

	tb.data = make(chan interface{})
	tb.cancel = make(chan bool, 1)
	tb.connections = make([]net.Conn, 1)
	tb.payload.DNCReports = make([]DNCReport, 0)
	tb.payload.CNIReports = make([]CNIReport, 0)
	tb.payload.NPMReports = make([]NPMReport, 0)
	tb.payload.CNSReports = make([]CNSReport, 0)

	err := telemetryLogger.SetTarget(log.TargetLogfile)
	if err != nil {
		fmt.Printf("Failed to configure logging: %v\n", err)
	}

	return &tb
}

// Starts Telemetry server listening on unix domain socket
func (tb *TelemetryBuffer) StartServer() error {
	err := tb.Listen(FdName)
	if err != nil {
		tb.FdExists = strings.Contains(err.Error(), "in use") || strings.Contains(err.Error(), "Access is denied")
		return err
	}

	// Spawn server goroutine to handle incoming connections
	go func() {
		for {
			// Spawn worker goroutines to communicate with client
			conn, err := tb.listener.Accept()
			if err == nil {
				tb.connections = append(tb.connections, conn)
				go func() {
					for {
						reportStr, err := read(conn)
						if err == nil {
							var tmp map[string]interface{}
							json.Unmarshal(reportStr, &tmp)
							if _, ok := tmp["NpmVersion"]; ok {
								var npmReport NPMReport
								json.Unmarshal([]byte(reportStr), &npmReport)
								tb.data <- npmReport
							} else if _, ok := tmp["CniSucceeded"]; ok {
								telemetryLogger.Printf("[Telemetry] Got cni report")
								var cniReport CNIReport
								json.Unmarshal([]byte(reportStr), &cniReport)
								tb.data <- cniReport
							} else if _, ok := tmp["Allocations"]; ok {
								var dncReport DNCReport
								json.Unmarshal([]byte(reportStr), &dncReport)
								tb.data <- dncReport
							} else if _, ok := tmp["DncPartitionKey"]; ok {
								var cnsReport CNSReport
								json.Unmarshal([]byte(reportStr), &cnsReport)
								tb.data <- cnsReport
							}
						}
					}
				}()
			}
		}
	}()

	return nil
}

func (tb *TelemetryBuffer) Connect() error {
	err := tb.Dial(FdName)
	if err == nil {
		tb.Connected = true
	} else if tb.FdExists {
		tb.Cleanup(FdName)
	}

	return err
}

// BufferAndPushData - BufferAndPushData running an instance if it isn't already being run elsewhere
func (tb *TelemetryBuffer) BufferAndPushData(intervalms time.Duration) {
	defer tb.close()
	if !tb.FdExists {
		telemetryLogger.Printf("[Telemetry] Buffer telemetry data and send it to host")
		if intervalms < DefaultInterval {
			intervalms = DefaultInterval
		}

		interval := time.NewTicker(intervalms).C
		for {
			select {
			case <-interval:
				// Send payload to host and clear cache when sent successfully
				// To-do : if we hit max slice size in payload, write to disk and process the logs on disk on future sends
				telemetryLogger.Printf("[Telemetry] send data to host")
				if err := tb.sendToHost(); err == nil {
					tb.payload.reset()
				} else {
					telemetryLogger.Printf("[Telemetry] sending to host failed with error %+v", err)
				}
			case report := <-tb.data:
				telemetryLogger.Printf("[Telemetry] Got data..Append it to buffer")
				tb.payload.push(report)
			case <-tb.cancel:
				goto EXIT
			}
		}
	} else {
		<-tb.cancel
	}

EXIT:
}

// read - read from the file descriptor
func read(conn net.Conn) (b []byte, err error) {
	b, err = bufio.NewReader(conn).ReadBytes(Delimiter)
	if err == nil {
		b = b[:len(b)-1]
	}

	return
}

// Write - write to the file descriptor
func (tb *TelemetryBuffer) Write(b []byte) (c int, err error) {
	b = append(b, Delimiter)
	w := bufio.NewWriter(tb.client)
	c, err = w.Write(b)
	if err == nil {
		err = w.Flush()
	}

	return
}

// Cancel - signal to tear down telemetry buffer
func (tb *TelemetryBuffer) Cancel() {
	tb.cancel <- true
}

// close - close all connections
func (tb *TelemetryBuffer) close() {
	if tb.client != nil {
		tb.client.Close()
	}

	if tb.listener != nil {
		tb.listener.Close()
	}

	for _, conn := range tb.connections {
		if conn != nil {
			conn.Close()
		}
	}
}

// sendToHost - send payload to host
func (tb *TelemetryBuffer) sendToHost() error {
	httpc := &http.Client{}
	var body bytes.Buffer
	telemetryLogger.Printf("Sending payload %+v", tb.payload)
	json.NewEncoder(&body).Encode(tb.payload)
	resp, err := httpc.Post(tb.azureHostReportURL, ContentType, &body)
	if err != nil {
		return fmt.Errorf("[Telemetry] HTTP Post returned error %v", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("[Telemetry] HTTP Post returned statuscode %d", resp.StatusCode)
	}

	return nil
}

// push - push the report (x) to corresponding slice
func (pl *Payload) push(x interface{}) {
	metadata, err := getHostMetadata()
	if err != nil {
		telemetryLogger.Printf("Error getting metadata %v", err)
	} else {
		err = saveHostMetadata(metadata)
		if err != nil {
			telemetryLogger.Printf("saving host metadata failed with :%v", err)
		}
	}

  if pl.len() < MaxPayloadSize {
    switch x.(type) {
    case DNCReport:
      dncReport := x.(DNCReport)
      dncReport.Metadata = metadata
      pl.DNCReports = append(pl.DNCReports, dncReport)
    case CNIReport:
      cniReport := x.(CNIReport)
      cniReport.Metadata = metadata
      pl.CNIReports = append(pl.CNIReports, cniReport)
    case NPMReport:
      npmReport := x.(NPMReport)
      npmReport.Metadata = metadata
      pl.NPMReports = append(pl.NPMReports, npmReport)
    case CNSReport:
      cnsReport := x.(CNSReport)
      cnsReport.Metadata = metadata
      pl.CNSReports = append(pl.CNSReports, cnsReport)
    }
  }
}

// reset - reset payload slices
func (pl *Payload) reset() {
	pl.DNCReports = nil
	pl.DNCReports = make([]DNCReport, 0)
	pl.CNIReports = nil
	pl.CNIReports = make([]CNIReport, 0)
	pl.NPMReports = nil
	pl.NPMReports = make([]NPMReport, 0)
	pl.CNSReports = nil
	pl.CNSReports = make([]CNSReport, 0)
}

// len - get number of payload items
func (pl *Payload) len() int {
	return len(pl.CNIReports) + len(pl.CNSReports) + len(pl.DNCReports) + len(pl.NPMReports)
}

// saveHostMetadata - save metadata got from wireserver to json file
func saveHostMetadata(metadata Metadata) error {
	dataBytes, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("[Telemetry] marshal data failed with err %+v", err)
	}

	if err = ioutil.WriteFile(metadataFile, dataBytes, 0644); err != nil {
		telemetryLogger.Printf("[Telemetry] Writing metadata to file failed: %v", err)
	}

	return err
}

// getHostMetadata - retrieve metadata from host
func getHostMetadata() (Metadata, error) {
	content, err := ioutil.ReadFile(metadataFile)
	if err == nil {
		var metadata Metadata
		if err = json.Unmarshal(content, &metadata); err == nil {
			telemetryLogger.Printf("[Telemetry] Returning hostmetadata from state")
			return metadata, nil
		}
	}

	req, err := http.NewRequest("GET", metadataURL, nil)
	if err != nil {
		return Metadata{}, err
	}

	req.Header.Set("Metadata", "True")
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return Metadata{}, err
	}

	defer resp.Body.Close()

	metareport := metadataWrapper{}

	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("[Telemetry] Request failed with HTTP error %d", resp.StatusCode)
	} else if resp.Body != nil {
		err = json.NewDecoder(resp.Body).Decode(&metareport)
		if err != nil {
			err = fmt.Errorf("[Telemetry] Unable to decode response body due to error: %s", err.Error())
		}
	} else {
		err = fmt.Errorf("[Telemetry] Response body is empty")
	}

	return metareport.Metadata, err
}

// StartTelemetryService - Kills if any telemetry service runs and start new telemetry service
func StartTelemetryService() error {
	platform.KillProcessByName(telemetryServiceProcessName)

	telemetryLogger.Printf("[Telemetry] Starting telemetry service process")
	path := fmt.Sprintf("%v/%v", cniInstallDir, telemetryServiceProcessName)
	if err := common.StartProcess(path); err != nil {
		telemetryLogger.Printf("[Telemetry] Failed to start telemetry service process :%v", err)
		return err
	}

	telemetryLogger.Printf("[Telemetry] Telemetry service started")

	for attempt := 0; attempt < 5; attempt++ {
		if checkIfSockExists() {
			break
		}

		time.Sleep(200 * time.Millisecond)
	}

	return nil
}
