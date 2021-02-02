package main

import (
	"app/webserver"
	"app/websockets"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/buger/jsonparser"
	"github.com/grumpypixel/msfs2020-simconnect-go/simconnect"
	// log "github.com/sirupsen/logrus"
)

type Params struct {
	connectionName string
	searchPath     string
	serverAddress  string
	timeout        int64
}

type Message struct {
	Type  string                 `json:"type"`
	Meta  string                 `json:"meta"`
	Data  map[string]interface{} `json:"data"`
	Debug string                 `json:"debug"`
}

const (
	appTitle                 = "MSFS2020-GoPilot"
	contentTypeHTML          = "text/html"
	contentTypeText          = "text/plain; charset=utf-8"
	defaultConnectionName    = "GoPilot"
	defaultServerAddress     = "0.0.0.0:8888"
	defaultSearchPath        = "."
	githubRoot               = "http://github.com/grumpypixel/msfs2020-gopilot/"
	githubReleases           = "http://github.com/grumpypixel/msfs2020-gopilot/releases"
	defaultConnectionTimeout = 600
	connectRetrySeconds      = 1
	requestDataInterval      = time.Millisecond * 250
	receiveDataInterval      = time.Millisecond * 1
)

var (
	params *Params
)

type App struct {
	simconnect.EventListener
	requestManager   *RequestManager
	socket           *websockets.WebSocket
	mate             *simconnect.SimMate
	flightSimVersion string
	done             chan bool
}

func init() {
	// log.SetFormatter(&log.TextFormatter{})
	// log.SetOutput(os.Stdout)
	// log.SetLevel(log.InfoLevel)
}

func main() {
	fmt.Printf("\nWelcome to %s\nProject page: %s\nReleases: %s\n\n", appTitle, githubRoot, githubReleases)
	parseParameters()
	locateLibrary(params.searchPath)

	app := NewApp()
	app.run(params)

	fmt.Println("Bye.")
}

func parseParameters() {
	params = &Params{}
	flag.StringVar(&params.connectionName, "name", defaultConnectionName, "SimConnect connection name")
	flag.StringVar(&params.searchPath, "searchpath", defaultSearchPath, "Additional DLL search path")
	flag.StringVar(&params.serverAddress, "address", defaultServerAddress, "Server address (<ipaddr>:<port>)")
	flag.Int64Var(&params.timeout, "timeout", defaultConnectionTimeout, "Timeout in seconds (optional)")
	flag.Parse()
}

func locateLibrary(additionalSearchPath string) {
	if simconnect.LocateLibrary(additionalSearchPath) == false {
		fullpath := path.Join(additionalSearchPath, simconnect.SimConnectDLL)
		fmt.Printf("DLL not found in given search paths\nUnpacking library to: %s\n", fullpath)
		if err := simconnect.UnpackDLL(fullpath); err != nil {
			fmt.Println("Unable to unpack DLL error:", err)
			return
		}
	}
}

func NewApp() *App {
	return &App{
		requestManager: NewRequestManager(),
		done:           make(chan bool, 1),
	}
}

func (app *App) run(params *Params) {
	app.socket = websockets.NewWebSocket()
	go app.handleSocketMessages()

	serverShutdown := make(chan bool, 1)
	defer close(serverShutdown)
	app.initWebServer(params.serverAddress, serverShutdown)

	if err := simconnect.Initialize(params.searchPath); err != nil {
		panic(err)
	}
	app.mate = simconnect.NewSimMate()
	if err := app.connect(params.connectionName, params.timeout); err != nil {
		fmt.Println(err)
		return
	}

	go app.handleTerminationSignal()
	go app.mate.HandleEvents(requestDataInterval, receiveDataInterval, app)

	defer close(app.done)
	<-app.done

	fmt.Println("Shutting down")
	if err := app.disconnect(); err != nil {
		panic(err)
	}
	serverShutdown <- true
}

func (app *App) initWebServer(address string, shutdown chan bool) {
	htmlHeaders := app.Headers(contentTypeHTML)
	textHeaders := app.Headers(contentTypeText)
	webServer := webserver.NewWebServer(address, shutdown)
	routes := []webserver.Route{
		{Pattern: "/", Handler: app.StaticContentHandler(htmlHeaders, "/", filepath.Join("assets/html", "vfrmap.html"))},
		{Pattern: "/vfrmap", Handler: app.StaticContentHandler(htmlHeaders, "/vfrmap", filepath.Join("assets/html", "vfrmap.html"))},
		{Pattern: "/mehmap", Handler: app.StaticContentHandler(htmlHeaders, "/mehmap", filepath.Join("assets/html", "mehmap.html"))},
		{Pattern: "/setdata", Handler: app.StaticContentHandler(htmlHeaders, "/setdata", filepath.Join("assets/html", "setdata.html"))},
		{Pattern: "/teleport", Handler: app.StaticContentHandler(htmlHeaders, "/teleport", filepath.Join("assets/html", "teleporter.html"))},
		{Pattern: "/debug", Handler: app.GeneratedContentHandler(textHeaders, "/debug", app.DebugGenerator)},
		{Pattern: "/simvars", Handler: app.GeneratedContentHandler(textHeaders, "/simvars", app.SimvarsGenerator)},
		{Pattern: "/ws", Handler: app.socket.Serve},
	}
	assetsDir := "/assets/"
	webServer.Run(routes, assetsDir)

	fmt.Println("Web Server listening on", address)
	fmt.Println("Your network interfaces:")
	webServer.ListNetworkInterfaces()
}

func (app *App) connect(name string, timeoutSeconds int64) error {
	fmt.Printf("Connecting to the Simulator... interval=%ds, timeout=%ds\n", connectRetrySeconds, timeoutSeconds)
	connectTicker := time.NewTicker(time.Second * time.Duration(connectRetrySeconds))
	defer connectTicker.Stop()

	timeoutTimer := time.NewTimer(time.Second * time.Duration(timeoutSeconds))
	defer timeoutTimer.Stop()

	count := 0
	for {
		select {
		case <-connectTicker.C:
			count++
			if err := app.mate.Open(name); err != nil {
				if count%10 == 0 {
					fmt.Printf("Connection attempts... %d\n", count)
				}
			} else {
				return nil
			}

		case <-timeoutTimer.C:
			return fmt.Errorf("Could not open a connection to the simulator within %d seconds", timeoutSeconds)
		}
	}
}

func (app *App) disconnect() error {
	fmt.Println("Closing connection.")
	if err := app.mate.Close(); err != nil {
		return err
	}
	return nil
}

func (app *App) handleTerminationSignal() {
	sigterm := make(chan os.Signal, 1)
	defer close(sigterm)

	signal.Notify(sigterm, os.Interrupt, syscall.SIGTERM)

	for {
		select {
		case <-sigterm:
			fmt.Println("Received SIGTERM.")
			app.done <- true
			return
		}
	}
}

func (app *App) handleSocketMessages() {
	for {
		select {
		case event := <-app.socket.EventReceiver:
			eventType := event.Type
			switch eventType {
			case websockets.SocketEventConnected:
				fmt.Println("Client connected:", event.Connection.UUID())

			case websockets.SocketEventDisconnected:
				fmt.Println("Client disconnected:", event.Connection.UUID())
				app.removeRequests(event.Connection.UUID())

			case websockets.SocketEventMessage:
				msg := &Message{}
				json.Unmarshal(event.Data, msg)
				switch msg.Type {
				case "register":
					app.handleRegisterMessage(msg, event.Data, event.Connection.UUID())

				case "deregister":
					app.handleDeregisterMessage(msg, event.Connection.UUID())

				case "setdata":
					app.handleSetDataMessage(msg)

				case "teleport":
					app.handleTeleportMessage(msg)

				default:
					fmt.Println("Received unhandled message:", msg.Type, msg.Data)
				}
			}
		}
	}
}

func (app *App) handleRegisterMessage(msg *Message, raw []byte, connID string) {
	fmt.Println("Registering client", connID)
	request := NewRequest(connID, msg.Meta)
	jsonparser.ArrayEach(raw, func(value []byte, dataType jsonparser.ValueType, offset int, err error) {
		n, _, _, _ := jsonparser.Get(value, "name")
		u, _, _, _ := jsonparser.Get(value, "unit")
		t, _, _, _ := jsonparser.Get(value, "type")
		m, _, _, _ := jsonparser.Get(value, "moniker")
		name := string(n)
		unit := string(u)
		typ := simconnect.StringToDataType(string(t))
		moniker := string(m)
		defineID := app.mate.AddSimVar(name, unit, typ)
		request.Add(defineID, name, moniker)
	}, "data")
	app.requestManager.AddRequest(request)
	fmt.Println("Added request", request)
}

func (app *App) handleDeregisterMessage(msg *Message, connID string) {
	fmt.Println("Deregistering", connID)
	app.removeRequests(connID)
}

func (app *App) handleTeleportMessage(msg *Message) {
	latitude := msg.Data["latitude"].(float64)
	longitude := msg.Data["longitude"].(float64)
	altitude := msg.Data["altitude"].(float64)
	heading := msg.Data["heading"].(float64)
	airspeed := msg.Data["airspeed"].(float64)
	bank := 0.0
	pitch := 0.0

	app.mate.SetSimObjectData("PLANE LATITUDE", "degrees", latitude, simconnect.DataTypeFloat64)
	app.mate.SetSimObjectData("PLANE LONGITUDE", "degrees", longitude, simconnect.DataTypeFloat64)
	app.mate.SetSimObjectData("PLANE ALTITUDE", "feet", altitude, simconnect.DataTypeFloat64)
	app.mate.SetSimObjectData("PLANE HEADING DEGREES TRUE", "degrees", heading, simconnect.DataTypeFloat64)
	app.mate.SetSimObjectData("AIRSPEED TRUE", "knot", airspeed, simconnect.DataTypeFloat64)
	app.mate.SetSimObjectData("PLANE BANK DEGREES", "degrees", bank, simconnect.DataTypeFloat64)
	app.mate.SetSimObjectData("PLANE PITCH DEGREES", "degrees", pitch, simconnect.DataTypeFloat64)

	fmt.Printf("Teleporting lat: %f lng: %f alt: %f hdg: %f spd: %f bnk: %f pit: %f\n",
		latitude, longitude, altitude, heading, airspeed, bank, pitch)
}

func (app *App) handleSetDataMessage(msg *Message) {
	fmt.Println("SetDataMessage", *msg)

	name := msg.Data["name"].(string)
	unit := msg.Data["unit"].(string)
	value := msg.Data["value"].(float64)

	if err := app.mate.SetSimObjectData(name, unit, value, simconnect.DataTypeFloat64); err != nil {
		fmt.Println(err)
	}
}

func (app *App) removeRequests(connID string) {
	temp := make([]*Request, 0)
	removed := make([]*Request, 0)
	for _, request := range app.requestManager.Requests {
		if request.ClientID != connID {
			temp = append(temp, request)
		} else {
			removed = append(removed, request)
		}
	}
	app.requestManager.Requests = temp
	for _, request := range removed {
		for defineID, v := range request.Vars {
			count := app.requestManager.RefCount(v.Name)
			if count == 0 {
				app.mate.RemoveSimVar(defineID)
			}
		}
	}
}

func (app *App) OnOpen(applName, applVersion, applBuild, simConnectVersion, simConnectBuild string) {
	fmt.Println("\nConnected.")
	app.flightSimVersion = fmt.Sprintf(
		"Flight Simulator:\n Name: %s\n Version: %s (build %s)\n SimConnect: %s (build %s)",
		applName, applVersion, applBuild, simConnectVersion, simConnectBuild)
	fmt.Printf("\n%s\n\n", app.flightSimVersion)
	fmt.Printf("CLEAR PROP!\n\n")
}

func (app *App) OnQuit() {
	fmt.Println("Disconnected.")
	app.done <- true
}

func (app *App) OnEventID(eventID simconnect.DWord) {
	fmt.Println("Received event ID", eventID)
}

func (app *App) OnException(exceptionCode simconnect.DWord) {
	fmt.Printf("Exception (code: %d)\n", exceptionCode)
}

func (app *App) OnDataUpdate(defineID simconnect.DWord) {
	// ignore
}

func (app *App) OnDataReady() {
	for _, request := range app.requestManager.Requests {
		msg := map[string]interface{}{"type": "simvars"}
		msg["type"] = "simvars"
		msg["meta"] = request.Meta

		vars := make(map[string]interface{})
		for defineID, v := range request.Vars {
			value, dataType, ok := app.mate.SimVarValueAndDataType(defineID)
			if !ok || value == nil {
				continue
			}
			switch dataType {
			case simconnect.DataTypeInt32:
				vars[v.Moniker] = simconnect.ValueToInt32(value)

			case simconnect.DataTypeInt64:
				vars[v.Moniker] = simconnect.ValueToInt64(value)

			case simconnect.DataTypeFloat32:
				vars[v.Moniker] = simconnect.ValueToFloat32(value)

			case simconnect.DataTypeFloat64:
				vars[v.Moniker] = simconnect.ValueToFloat64(value)

			case simconnect.DataTypeString8,
				simconnect.DataTypeString32,
				simconnect.DataTypeString64,
				simconnect.DataTypeString128,
				simconnect.DataTypeString256,
				simconnect.DataTypeString260,
				simconnect.DataTypeStringV:
				vars[v.Moniker] = simconnect.ValueToString(value)
			}
		}
		msg["data"] = vars
		recipient := request.ClientID
		if buf, err := json.Marshal(msg); err == nil {
			app.socket.Send(recipient, buf)
		}
	}
	if err := app.BroadcastStatusMessage(); err != nil {
		fmt.Println(err)
	}
}

func (app *App) BroadcastStatusMessage() error {
	data := map[string]interface{}{"simconnect": app.mate.IsConnected()}
	msg := map[string]interface{}{"type": "status", "data": data}
	buf, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	app.socket.Broadcast(buf)
	return nil
}

func (app *App) Headers(contentType string) map[string]string {
	headers := map[string]string{
		"Access-Control-Allow-Origin": "*",
		"Cache-Control":               "no-cache, no-store, must-revalidate",
		"Pragma":                      "no-cache",
		"Expires":                     "0",
		"Content-Type":                contentType,
	}
	return headers
}

func (app *App) StaticContentHandler(headers map[string]string, urlPath, filePath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != urlPath {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		for key, value := range headers {
			w.Header().Set(key, value)
		}
		http.ServeFile(w, r, filePath)
	}
}

func (app *App) GeneratedContentHandler(headers map[string]string, urlPath string, generator func(w http.ResponseWriter)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != urlPath {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		for key, value := range headers {
			w.Header().Set(key, value)
		}
		w.WriteHeader(http.StatusOK)
		generator(w)
	}
}

func (app *App) SimvarsGenerator(w http.ResponseWriter) {
	fmt.Fprintf(w, "%s\n\n", appTitle)
	fmt.Fprintf(w, "%s\n", app.DumpedSimVars())
}

func (app *App) DebugGenerator(w http.ResponseWriter) {
	fmt.Fprintf(w, "%s\n\n", appTitle)
	if len(app.flightSimVersion) > 0 {
		fmt.Fprintf(w, "%s\n\n", app.flightSimVersion)
	}
	fmt.Fprintf(w, "SimConnect\n  initialized: %v\n  conncected: %v\n\n", simconnect.IsInitialized(), app.mate.IsConnected())
	fmt.Fprintf(w, "Clients: %d\n", app.socket.ConnectionCount())
	uuids := app.socket.ConnectionUUIDs()
	for i, uuid := range uuids {
		fmt.Fprintf(w, "  %02d: %s\n", i, uuid)
	}
	fmt.Fprintf(w, "\n")
	fmt.Fprintf(w, "%s\n\n", app.DumpedSimVars())
	fmt.Fprintf(w, "%s\n", app.DumpedRequests())
}

func (app *App) DumpedRequests() string {
	var dump string
	dump += fmt.Sprintf("Requests: %d\n", app.requestManager.RequestCount())
	for i, request := range app.requestManager.Requests {
		dump += fmt.Sprintf("  %02d: Client: %s Vars: %d Meta: %s\n", i+1, request.ClientID, len(request.Vars), request.Meta)
		count := 1
		for name, moniker := range request.Vars {
			dump += fmt.Sprintf("    %02d: name: %s moniker: %s\n", count, name, moniker)
			count++
		}
	}
	return dump
}

func (app *App) DumpedSimVars() string {
	indent := "  "
	dump := app.mate.SimVarDump(indent)
	str := strings.Join(dump[:], "\n")
	return fmt.Sprintf("SimVars: %d\n", len(dump)) + str
}

// func (app *App) handleSetCameraMessage(msg *Message) {
// 	fmt.Println("SetCameraMessage", *msg)
// 	deltaX := msg.Data["delta_x"].(float64)
// 	deltaY := msg.Data["delta_y"].(float64)
// 	deltaZ := msg.Data["delta_z"].(float64)
// 	pitch := msg.Data["pitch"].(float64)
// 	bank := msg.Data["bank"].(float64)
// 	heading := msg.Data["heading"].(float64)
// 	app.mate.CameraSetRelative6DOF(deltaX, deltaY, deltaZ, pitch, bank, heading)
// }

// func (app *App) handleSetTextMessage(msg *Message) {
// 	fmt.Println("SetTextMessage", *msg)
// 	// IMPLEMENT ME
// 	text := "HELLO, SIMWORLD!"
// 	textType := simconnect.TextTypePrintMagenta
// 	duration := 10.0
// 	eventID := simconnect.NextEventID()
// 	app.mate.Text(text, textType, duration, eventID)
// }
