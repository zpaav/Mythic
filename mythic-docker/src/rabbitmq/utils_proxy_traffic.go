package rabbitmq

import (
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/its-a-feature/Mythic/database"
	"github.com/its-a-feature/Mythic/database/enums/InteractiveTask"
	databaseStructs "github.com/its-a-feature/Mythic/database/structs"
	"github.com/its-a-feature/Mythic/logging"
	"github.com/its-a-feature/Mythic/utils"
	"math"
	"math/rand"
	"net"
	"sort"
	"time"
)

type CallbackPortType = string

const (
	CALLBACK_PORT_TYPE_SOCKS       CallbackPortType = "socks"
	CALLBACK_PORT_TYPE_RPORTFWD                     = "rpfwd"
	CALLBACK_PORT_TYPE_INTERACTIVE                  = "interactive"
)

type proxyToAgentMessage struct {
	ServerID uint32 `json:"server_id" mapstructure:"server_id"`
	Message  []byte `json:"data" mapstructure:"data"`
	IsExit   bool   `json:"exit" mapstructure:"exit"`
}
type proxyFromAgentMessage struct {
	ServerID uint32 `json:"server_id" mapstructure:"server_id"`
	Message  string `json:"data" mapstructure:"data"`
	IsExit   bool   `json:"exit" mapstructure:"exit"`
}
type acceptedConnection struct {
	conn                         net.Conn
	shouldClose                  chan bool
	messagesFromAgent            chan proxyFromAgentMessage
	interactiveMessagesFromAgent chan agentMessagePostResponseInteractive
	ServerID                     uint32
	TaskUUID                     *string
}

type callbackPortUsage struct {
	CallbackID                 int              `json:"callback_id" db:"callback_id"`
	TaskID                     int              `json:"task_id" db:"task_id"`
	LocalPort                  int              `json:"local_port" db:"local_port"`
	RemotePort                 int              `json:"remote_port" db:"remote_port"`
	RemoteIP                   string           `json:"remote_ip" db:"remote_ip"`
	OperationID                int              `json:"operation_id" db:"operation_id"`
	PortType                   CallbackPortType `json:"port_type" db:"port_type"`
	listener                   *net.Listener
	acceptedConnections        *[]*acceptedConnection
	messagesToAgent            chan proxyToAgentMessage
	interactiveMessagesToAgent chan agentMessagePostResponseInteractive
	newConnectionChannel       chan *acceptedConnection
	removeConnectionsChannel   chan *acceptedConnection
	// messagesFromAgent - these get parsed by manageConnections and passed to the right connection's messagesFromAgent
	messagesFromAgent            chan proxyFromAgentMessage
	interactiveMessagesFromAgent chan agentMessagePostResponseInteractive
	stopAllConnections           chan bool
}

type callbackPortsInUse struct {
	ports                        []*callbackPortUsage
	proxyFromAgentMessageChannel chan ProxyFromAgentMessageForMythic
}

var proxyPorts callbackPortsInUse

type ProxyStopResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
}
type ProxyStop struct {
	CallbackPortId    int
	Action            string
	OperatorOperation databaseStructs.Operatoroperation
}
type ProxyFromAgentMessageForMythic struct {
	CallbackID          int
	PortType            CallbackPortType
	Messages            []proxyFromAgentMessage
	InteractiveMessages []agentMessagePostResponseInteractive
}

func ManuallyToggleProxy(input ProxyStop) ProxyStopResponse {
	resp := ProxyStopResponse{
		Status: "error",
		Error:  "callback and port not found",
	}
	callbackPort := databaseStructs.Callbackport{}
	err := database.DB.Get(&callbackPort, `SELECT * FROM callbackport 
         WHERE id=$1 AND operation_id=$2`, input.CallbackPortId, input.OperatorOperation.OperationID)
	if err != nil {
		logging.LogError(err, "Failed to find callback port information for manual proxy toggle")
		resp.Error = "Failed to find callback port information"
		return resp
	}
	if input.Action == "start" {
		err = proxyPorts.Add(callbackPort.CallbackID, callbackPort.PortType,
			callbackPort.LocalPort, callbackPort.RemotePort, callbackPort.RemoteIP,
			callbackPort.TaskID, input.OperatorOperation.CurrentOperation.ID)
		if err != nil {
			resp.Error = err.Error()
			return resp
		}
		resp.Status = "success"
		resp.Error = ""
		_, err = database.DB.NamedExec(`UPDATE callbackport SET deleted=false WHERE id=:id`, callbackPort)
		if err != nil {
			logging.LogError(err, "Failed to update database to mark callback port as not deleted")
		}
		return resp

	} else if input.Action == "stop" {
		err = proxyPorts.Remove(callbackPort.CallbackID, callbackPort.PortType, callbackPort.LocalPort, input.OperatorOperation.CurrentOperation.ID)
		if err != nil {
			resp.Error = err.Error()
		}
		resp.Status = "success"
		resp.Error = ""
		_, err = database.DB.NamedExec(`UPDATE callbackport SET deleted=true WHERE id=:id`, callbackPort)
		if err != nil {
			logging.LogError(err, "Failed to update database to mark callback port as deleted")
		}
		return resp

	} else {
		resp.Error = "Unknown action, expected 'start' or 'stop'"
		return resp
	}
}
func (c *callbackPortsInUse) Initialize() {
	callbackPorts := []databaseStructs.Callbackport{}
	c.ports = make([]*callbackPortUsage, 0)
	c.proxyFromAgentMessageChannel = make(chan ProxyFromAgentMessageForMythic, 100)
	go c.ListenForProxyFromAgentMessage()
	if err := database.DB.Select(&callbackPorts, `SELECT * FROM callbackport WHERE deleted=false`); err != nil {
		logging.LogError(err, "Failed to load callback ports from database")
	} else {
		for _, proxy := range callbackPorts {
			newPort := callbackPortUsage{
				CallbackID:                   proxy.CallbackID,
				TaskID:                       proxy.TaskID,
				LocalPort:                    proxy.LocalPort,
				RemoteIP:                     proxy.RemoteIP,
				RemotePort:                   proxy.RemotePort,
				PortType:                     proxy.PortType,
				OperationID:                  proxy.OperationID,
				messagesToAgent:              make(chan proxyToAgentMessage, 200),
				newConnectionChannel:         make(chan *acceptedConnection, 200),
				removeConnectionsChannel:     make(chan *acceptedConnection, 200),
				messagesFromAgent:            make(chan proxyFromAgentMessage, 200),
				interactiveMessagesToAgent:   make(chan agentMessagePostResponseInteractive, 200),
				interactiveMessagesFromAgent: make(chan agentMessagePostResponseInteractive, 200),
				stopAllConnections:           make(chan bool),
			}
			acceptedConnections := make([]*acceptedConnection, 0)
			newPort.acceptedConnections = &acceptedConnections
			if err := newPort.Start(); err != nil {
				logging.LogError(err, "Failed to start listening", "port info", &newPort)
			} else {
				c.ports = append(c.ports, &newPort)
			}
		}
	}
}
func (c *callbackPortsInUse) ListenForProxyFromAgentMessage() {
	for {
		agentMessage := <-c.proxyFromAgentMessageChannel
		for i := 0; i < len(c.ports); i++ {
			if c.ports[i].CallbackID == agentMessage.CallbackID && c.ports[i].PortType == agentMessage.PortType {
				switch agentMessage.PortType {
				case CALLBACK_PORT_TYPE_RPORTFWD:
					fallthrough
				case CALLBACK_PORT_TYPE_SOCKS:
					for j := 0; j < len(agentMessage.Messages); j++ {
						//logging.LogInfo("got message from agent", "chan", messages[j].ServerID, "p.messagesFromAgentQueue", len(c.ports[i].messagesFromAgent))
						c.ports[i].messagesFromAgent <- agentMessage.Messages[j]
					}
				case CALLBACK_PORT_TYPE_INTERACTIVE:
					for j := 0; j < len(agentMessage.InteractiveMessages); j++ {
						//logging.LogInfo("got message from agent", "chan", messages[j].ServerID, "p.messagesFromAgentQueue", len(c.ports[i].messagesFromAgent))
						c.ports[i].interactiveMessagesFromAgent <- agentMessage.InteractiveMessages[j]
					}
					handleAgentMessagePostResponseInteractiveOutput(&agentMessage.InteractiveMessages)
				}

			}
		}
	}
}
func (c *callbackPortsInUse) GetNextAvailableLocalPort() uint32 {
	exposedPorts := utils.MythicConfig.ServerDynamicPorts
	sort.Slice(exposedPorts, func(i, j int) bool {
		return exposedPorts[i] < exposedPorts[j]
	})
	for _, port := range exposedPorts {
		if !c.IsPortInUse(port) {
			return port
		}
	}
	return 0
}
func (c *callbackPortsInUse) IsPortInUse(port uint32) bool {
	for i, _ := range c.ports {
		switch c.ports[i].PortType {
		case CALLBACK_PORT_TYPE_RPORTFWD:
			// rpfwd doesn't bind a port on Mythic, so the ports don't count here
			continue
		case CALLBACK_PORT_TYPE_SOCKS:
			fallthrough
		case CALLBACK_PORT_TYPE_INTERACTIVE:
			if uint32(c.ports[i].LocalPort) == port {
				return true
			}
		}
	}
	return false
}
func (c *callbackPortsInUse) GetPortForTypeAndCallback(taskId int, callbackId int, portType CallbackPortType) int {
	for i, _ := range c.ports {
		switch portType {
		case CALLBACK_PORT_TYPE_SOCKS:
			if c.ports[i].PortType == portType && c.ports[i].CallbackID == callbackId {
				return c.ports[i].LocalPort
			}
		case CALLBACK_PORT_TYPE_RPORTFWD:
			fallthrough
		case CALLBACK_PORT_TYPE_INTERACTIVE:
			if c.ports[i].PortType == portType && c.ports[i].CallbackID == callbackId && c.ports[i].TaskID == taskId {
				return c.ports[i].LocalPort
			}
		}

	}
	return 0
}
func (c *callbackPortsInUse) GetDataForCallbackIdPortType(callbackId int, portType CallbackPortType) (interface{}, error) {
	var interactiveData []agentMessagePostResponseInteractive
	var socksData []proxyToAgentMessage
	for i := 0; i < len(c.ports); i++ {
		if c.ports[i].CallbackID == callbackId && c.ports[i].PortType == portType {
			switch portType {
			case CALLBACK_PORT_TYPE_INTERACTIVE:
				interactiveData = append(interactiveData, c.ports[i].GetData().([]agentMessagePostResponseInteractive)...)
			case CALLBACK_PORT_TYPE_SOCKS:
				fallthrough
			case CALLBACK_PORT_TYPE_RPORTFWD:
				socksData = append(socksData, c.ports[i].GetData().([]proxyToAgentMessage)...)
			}

		}
	}
	switch portType {
	case CALLBACK_PORT_TYPE_INTERACTIVE:
		if len(interactiveData) > 0 {
			return interactiveData, nil
		}
	case CALLBACK_PORT_TYPE_SOCKS:
		fallthrough
	case CALLBACK_PORT_TYPE_RPORTFWD:
		if len(socksData) > 0 {
			return socksData, nil
		}
	}

	return nil, nil
}
func (c *callbackPortsInUse) SendDataToCallbackIdPortType(callbackId int, portType CallbackPortType, messages []proxyFromAgentMessage) {
	c.proxyFromAgentMessageChannel <- ProxyFromAgentMessageForMythic{
		CallbackID: callbackId,
		PortType:   portType,
		Messages:   messages,
	}
}
func (c *callbackPortsInUse) SendInteractiveDataToCallbackIdPortType(callbackId int, portType CallbackPortType, messages []agentMessagePostResponseInteractive) {
	c.proxyFromAgentMessageChannel <- ProxyFromAgentMessageForMythic{
		CallbackID:          callbackId,
		PortType:            portType,
		InteractiveMessages: messages,
	}
	/*
		for i := 0; i < len(c.ports); i++ {
			if c.ports[i].CallbackID == callbackId && c.ports[i].PortType == portType {
				for j := 0; j < len(messages); j++ {
					//logging.LogInfo("got message from agent", "chan", messages[j].ServerID, "p.messagesFromAgentQueue", len(c.ports[i].messagesFromAgent))
					c.ports[i].interactiveMessagesFromAgent <- messages[j]
				}
			}
		}
		go handleAgentMessagePostResponseInteractiveOutput(&messages)

	*/
}
func (c *callbackPortsInUse) GetOtherCallbackIds(callbackId int) []int {
	callbackIds := []int{}
	for i := 0; i < len(c.ports); i++ {
		if c.ports[i].CallbackID != callbackId {
			callbackIds = append(callbackIds, c.ports[i].CallbackID)
		}
	}
	return callbackIds
}
func (c *callbackPortsInUse) GetDataForCallbackId(callbackId int, portType string) (interface{}, error) {
	return c.GetDataForCallbackIdPortType(callbackId, portType)
}
func (c *callbackPortsInUse) Add(callbackId int, portType CallbackPortType, localPort int, remotePort int, remoteIP string, taskId int, operationId int) error {
	newPort := callbackPortUsage{
		CallbackID:                   callbackId,
		TaskID:                       taskId,
		LocalPort:                    localPort,
		RemotePort:                   remotePort,
		RemoteIP:                     remoteIP,
		OperationID:                  operationId,
		PortType:                     portType,
		messagesToAgent:              make(chan proxyToAgentMessage, 200),
		newConnectionChannel:         make(chan *acceptedConnection, 200),
		removeConnectionsChannel:     make(chan *acceptedConnection, 200),
		messagesFromAgent:            make(chan proxyFromAgentMessage, 200),
		interactiveMessagesToAgent:   make(chan agentMessagePostResponseInteractive, 200),
		interactiveMessagesFromAgent: make(chan agentMessagePostResponseInteractive, 200),
		stopAllConnections:           make(chan bool),
	}
	acceptedConnections := make([]*acceptedConnection, 0)
	newPort.acceptedConnections = &acceptedConnections
	err := newPort.Start()
	if err != nil {
		logging.LogError(err, "Failed to start new proxy port")
		return err
	}
	callbackPort := databaseStructs.Callbackport{}
	err = database.DB.Get(&callbackPort, `SELECT id FROM callbackport WHERE
                                operation_id=$1 AND callback_id=$2 AND local_port=$3 AND port_type=$4`,
		operationId, callbackId, localPort, portType)
	if err == sql.ErrNoRows {
		_, err = database.DB.NamedExec(`INSERT INTO callbackport 
		(task_id, operation_id, callback_id, local_port, port_type, remote_port, remote_ip)
		VALUES (:task_id, :operation_id, :callback_id, :local_port, :port_type, :remote_port, :remote_ip)`, &newPort)
	}
	if err == nil {
		_, err = database.DB.NamedExec(`UPDATE callbackport SET deleted=false WHERE id=:id`, callbackPort)
	}
	if err != nil {
		logging.LogError(err, "Failed to create new callback port mapping")
		if err := newPort.Stop(); err != nil {
			logging.LogError(err, "Failed to stop new callback port")
		}
		return err
	}
	c.ports = append(c.ports, &newPort)
	return nil

}
func (c *callbackPortsInUse) Remove(callbackId int, portType CallbackPortType, localPort int, operationId int) error {
	for i := 0; i < len(c.ports); i++ {
		if c.ports[i].CallbackID == callbackId &&
			c.ports[i].OperationID == operationId &&
			c.ports[i].PortType == portType &&
			c.ports[i].LocalPort == localPort {
			if err := c.ports[i].Stop(); err != nil {
				logging.LogError(err, "Failed to stop proxy")
				//c.Unlock()
				return err
			} else if _, err := database.DB.Exec(`UPDATE callbackport SET deleted=true WHERE
				callback_id=$1 AND local_port=$2 AND port_type=$3 AND operation_id=$4 AND deleted=false`,
				callbackId, localPort, portType, operationId); err != nil {
				logging.LogError(err, "Failed to delete port mapping from database for proxy")
				//c.Unlock()
				return err
			} else {
				c.ports = append((c.ports)[:i], (c.ports)[i+1:]...)
				//c.Unlock()
				return nil
			}
		}
	}
	//c.Unlock()
	return nil
}
func isPortExposedThroughDocker(portToCheck int) bool {
	for _, port := range utils.MythicConfig.ServerDynamicPorts {
		if port == uint32(portToCheck) {
			return true
		}
	}
	return false
}
func canReachRemoteHost(remoteIP string, remotePort int) bool {
	if conn, err := net.Dial("tcp", fmt.Sprintf("%s:%d", remoteIP, remotePort)); err != nil {
		logging.LogError(err, "Failed to connect to remote for rpfwd", "remote_ip", remoteIP, "remote port", remotePort)
		return false
	} else {
		conn.Close()
		return true
	}
}
func (p *callbackPortUsage) Start() error {
	switch p.PortType {
	case CALLBACK_PORT_TYPE_SOCKS:
		if isPortExposedThroughDocker(p.LocalPort) {
			addr := fmt.Sprintf("0.0.0.0:%d", p.LocalPort)
			if l, err := net.Listen("tcp", addr); err != nil {
				logging.LogError(err, "Failed to start listening on new port")
				go SendAllOperationsMessage(err.Error(), p.OperationID, "", database.MESSAGE_LEVEL_WARNING)
				return err
			} else {
				p.listener = &l
				go p.handleSocksConnections()
				go p.manageConnections()
				go SendAllOperationsMessage(fmt.Sprintf("Opened port %d for %s", p.LocalPort, p.PortType),
					p.OperationID, "", database.MESSAGE_LEVEL_INFO)
			}
		} else {
			err := errors.New(fmt.Sprintf("Failed to start listening on port %d, it's not exposed through docker", p.LocalPort))
			logging.LogError(err, "Can't start listening")
			go SendAllOperationsMessage(err.Error(), p.OperationID, "", database.MESSAGE_LEVEL_WARNING)
			return err
		}
	case CALLBACK_PORT_TYPE_RPORTFWD:
		// start managing incoming/outgoing connections
		go p.manageConnections()
		if !canReachRemoteHost(p.RemoteIP, p.RemotePort) {
			err := errors.New(fmt.Sprintf("Testing remote connection for rpfwd:\nfailed to reach remote host, %s:%d", p.RemoteIP, p.RemotePort))
			go SendAllOperationsMessage(err.Error(), p.OperationID, "", database.MESSAGE_LEVEL_WARNING)
		} else {
			go SendAllOperationsMessage(fmt.Sprintf("Testing remote connection for rpfwd:\nsuccessfully connected to remote host, %s:%d", p.RemoteIP, p.RemotePort),
				p.OperationID, "", database.MESSAGE_LEVEL_INFO)
		}
	case CALLBACK_PORT_TYPE_INTERACTIVE:
		if isPortExposedThroughDocker(p.LocalPort) {
			addr := fmt.Sprintf("0.0.0.0:%d", p.LocalPort)
			if l, err := net.Listen("tcp", addr); err != nil {
				logging.LogError(err, "Failed to start listening on new port")
				go SendAllOperationsMessage(err.Error(), p.OperationID, "", database.MESSAGE_LEVEL_WARNING)
				return err
			} else {
				p.listener = &l
				go p.handleInteractiveConnections()
				go p.manageConnections()
				go SendAllOperationsMessage(fmt.Sprintf("Opened port %d for %s", p.LocalPort, "interactive tasking"),
					p.OperationID, "", database.MESSAGE_LEVEL_INFO)
			}
		} else {
			err := errors.New(fmt.Sprintf("Failed to start listening on port %d, it's not exposed through docker", p.LocalPort))
			logging.LogError(err, "Can't start listening")
			go SendAllOperationsMessage(err.Error(), p.OperationID, "", database.MESSAGE_LEVEL_WARNING)
			return err
		}
	default:
		return errors.New("unknown proxy type")
	}
	return nil

}
func (p *callbackPortUsage) Stop() error {
	if p.listener != nil {
		if err := (*p.listener).Close(); err != nil {
			if err == net.ErrClosed {
				logging.LogInfo("tasking to stop all connections via channel")
				p.stopAllConnections <- true
				return nil
			}
			logging.LogError(err, "Error calling close for the listener in the Stop function")
			return err
		}
		logging.LogInfo("tasking to stop all connections via channel")
		p.stopAllConnections <- true
		return nil
	} else {
		logging.LogInfo("tasking to stop all connections via channel")
		p.stopAllConnections <- true
		return nil
	}

}
func (p *callbackPortUsage) GetData() interface{} {
	switch p.PortType {
	case CALLBACK_PORT_TYPE_SOCKS:
		fallthrough
	case CALLBACK_PORT_TYPE_RPORTFWD:
		return p.GetProxyData()
	case CALLBACK_PORT_TYPE_INTERACTIVE:
		return p.GetInteractiveData()
	}
	return nil
}
func (p *callbackPortUsage) GetProxyData() []proxyToAgentMessage {
	messagesToSendToAgent := make([]proxyToAgentMessage, len(p.messagesToAgent))
	for i := 0; i < len(messagesToSendToAgent); i++ {
		select {
		case messagesToSendToAgent[i] = <-p.messagesToAgent:
			//logging.LogDebug("Got message from Mythic to agent", "serverID", messagesToSendToAgent[i].ServerID)
		default:
			//logging.LogDebug("returning set of messages to agent from Mythic", "msgs", messagesToSendToAgent)
			// this is in case we run out of messages for some reason
			return messagesToSendToAgent
		}
	}
	return messagesToSendToAgent
}
func (p *callbackPortUsage) GetInteractiveData() []agentMessagePostResponseInteractive {
	messagesToSendToAgent := make([]agentMessagePostResponseInteractive, len(p.interactiveMessagesToAgent))
	for i := 0; i < len(messagesToSendToAgent); i++ {
		select {
		case messagesToSendToAgent[i] = <-p.interactiveMessagesToAgent:
			//logging.LogDebug("Got message from Mythic to agent", "TaskID", messagesToSendToAgent[i].TaskUUID)
		default:
			//logging.LogDebug("returning set of messages to agent from Mythic", "msgs", messagesToSendToAgent)
			// this is in case we run out of messages for some reason
			return messagesToSendToAgent
		}
	}
	if interactiveData, err := handleAgentMessageGetInteractiveTasking(p.CallbackID); err != nil {
		logging.LogError(err, "Failed to get interactive tasking data")
	} else if interactiveData != nil {
		messagesToSendToAgent = append(messagesToSendToAgent, interactiveData...)
	}
	return messagesToSendToAgent
}
func (p *callbackPortUsage) manageConnections() {
	connectionMap := map[uint32]*acceptedConnection{}
	for {
		select {
		case newConn := <-p.newConnectionChannel:
			//logging.LogInfo("adding new connection channel in manageConnections")
			connectionMap[newConn.ServerID] = newConn
		case removeCon := <-p.removeConnectionsChannel:
			logging.LogInfo("removing connection channel in manageConnection")
			if removeCon.TaskUUID != nil {
				// remove all connections for interactive task
				closeIDs := []uint32{}
				for serverID, _ := range connectionMap {
					if connectionMap[serverID].TaskUUID != nil && connectionMap[serverID].TaskUUID == removeCon.TaskUUID {
						//connectionMap[serverID].shouldClose <- true
						close(connectionMap[serverID].interactiveMessagesFromAgent)
						connectionMap[serverID].conn.Close()
						closeIDs = append(closeIDs, serverID)
					}
				}
				for _, serverID := range closeIDs {
					delete(connectionMap, serverID)
				}
			} else {
				if _, ok := connectionMap[removeCon.ServerID]; ok {
					connectionMap[removeCon.ServerID].shouldClose <- true
					close(connectionMap[removeCon.ServerID].messagesFromAgent)
					connectionMap[removeCon.ServerID].conn.Close()
					delete(connectionMap, removeCon.ServerID)
					interceptProxyToAgentMessageChan <- interceptProxyToAgentMessage{
						Message: proxyToAgentMessage{
							Message:  nil,
							IsExit:   true,
							ServerID: removeCon.ServerID,
						},
						MessagesToAgent: p.messagesToAgent,
						ProxyType:       p.PortType,
						CallbackID:      p.CallbackID,
					}
				}
			}

		case newMsg := <-p.messagesFromAgent:
			switch p.PortType {
			case CALLBACK_PORT_TYPE_SOCKS:
				//logging.LogInfo("got message from agent in p.messagesFromAgent", "chan", newMsg.ServerID)
				if _, ok := connectionMap[newMsg.ServerID]; ok {
					//logging.LogInfo("found supporting connection in connection map", "chan", newMsg.ServerID)
					connectionMap[newMsg.ServerID].messagesFromAgent <- newMsg
					//logging.LogInfo("send message along to acceptedConnection's messagesFromAgent", "chan", newMsg.ServerID)
				} else if !newMsg.IsExit {
					// we don't have knowledge of this ServerID and this isn't an "IsExit" message, so tell the other end to close
					interceptProxyToAgentMessageChan <- interceptProxyToAgentMessage{
						Message: proxyToAgentMessage{
							Message:  nil,
							IsExit:   true,
							ServerID: newMsg.ServerID,
						},
						MessagesToAgent: p.messagesToAgent,
						ProxyType:       p.PortType,
						CallbackID:      p.CallbackID,
					}
				} else {
					//logging.LogInfo("unknown server id in connections map for messagesFromAgent", "serverID", newMsg.ServerID)
				}
			case CALLBACK_PORT_TYPE_RPORTFWD:
				//logging.LogInfo("got message from agent in p.messagesFromAgent", "chan", newMsg.ServerID)
				if _, ok := connectionMap[newMsg.ServerID]; ok {
					//logging.LogInfo("found supporting connection in connection map", "chan", newMsg.ServerID)
					// this means that we've seen this ServerID before, established a remote connection, and are just sending more data
					connectionMap[newMsg.ServerID].messagesFromAgent <- newMsg
					//logging.LogInfo("send message along to acceptedConnection's messagesFromAgent", "chan", newMsg.ServerID)
				} else {
					// got a new serverID from the agent that we aren't tracking, so we need to make a new connection
					if conn, err := net.Dial("tcp", fmt.Sprintf("%s:%d", p.RemoteIP, p.RemotePort)); err != nil {
						logging.LogError(err, "Failed to connect to remote for rpfwd", "remote_ip", p.RemoteIP, "remote port", p.RemotePort)
						interceptProxyToAgentMessageChan <- interceptProxyToAgentMessage{
							Message: proxyToAgentMessage{
								Message:  nil,
								IsExit:   true,
								ServerID: newMsg.ServerID,
							},
							MessagesToAgent: p.messagesToAgent,
							CallbackID:      p.CallbackID,
							ProxyType:       p.PortType,
						}
					} else {
						// we have a valid connection to the remote server
						// Handle connections in a new goroutine
						newConnection := acceptedConnection{
							conn:              conn,
							shouldClose:       make(chan bool, 1),
							messagesFromAgent: make(chan proxyFromAgentMessage, 200),
							ServerID:          newMsg.ServerID, // randomized id
						}
						//p.newConnectionChannel <- &newConnection
						// using the channel like normal might cause a race condition with processing the next message from the agent
						connectionMap[newConnection.ServerID] = &newConnection
						//logging.LogInfo("made new connection to remote ip/port, about to handle connections")
						go p.handleRpfwdConnections(&newConnection)
						newConnection.messagesFromAgent <- newMsg
					}
				}
			default:

			}
		case newMsg := <-p.interactiveMessagesFromAgent:
			// find all connections for the specified task uuid
			for serverID, _ := range connectionMap {
				if connectionMap[serverID].TaskUUID != nil && *connectionMap[serverID].TaskUUID == newMsg.TaskUUID {
					connectionMap[serverID].interactiveMessagesFromAgent <- newMsg
				}
			}

		case <-p.stopAllConnections:
			for _, rmProxyData := range connectionMap {
				if rmProxyData.TaskUUID != nil {
					close(rmProxyData.interactiveMessagesFromAgent)
				} else {
					close(rmProxyData.messagesFromAgent)
				}

				rmProxyData.conn.Close()
				delete(connectionMap, rmProxyData.ServerID)
			}
			return
		case <-time.After(10 * time.Second):
			//logging.LogError(nil, "1s timeout, re-looping", "p.newConnectionChannel", len(p.newConnectionChannel),
			//	"p.removeConnectionsChannel", len(p.removeConnectionsChannel),
			//	"p.messagesFromAgent", len(p.messagesFromAgent))
		}

	}

}
func (p *callbackPortUsage) handleSocksConnections() {
	for {
		// Listen for an incoming connection
		if p.listener != nil {
			if conn, err := (*p.listener).Accept(); err != nil {
				logging.LogError(err, "Failed to accept new connection on port", "port", p.LocalPort)
				if err := (*p.listener).Close(); err != nil {
					logging.LogError(err, "Failed to close listener", "port", p.LocalPort)
				}
				//p.listener = nil
				return
			} else {
				// Handle connections in a new goroutine
				newConnection := acceptedConnection{
					conn:              conn,
					shouldClose:       make(chan bool, 1),
					messagesFromAgent: make(chan proxyFromAgentMessage, 200),
					ServerID:          uint32(rand.Intn(math.MaxInt32)), // randomized id
				}
				// add this connection for tracking so it can be cancelled later if needed

				// this reads from the connection and writes data for the agent to process
				initial := make([]byte, 4)
				if _, err := conn.Read(initial); err != nil {
					logging.LogError(err, "failed to read initial SOCKS connection data")
					return
				} else if _, err := conn.Write([]byte{'\x05', '\x00'}); err != nil {
					logging.LogError(err, "Failed to send the \\x05\\x00 no-auth bytes for new socks connection")
					return
				}
				p.newConnectionChannel <- &newConnection

				//logging.LogDebug("Got new connection", "server_id", newConnection.ServerID)
				go func(conn net.Conn) {
					// function for reading from agents to send to Mythic's connections
					for {
						select {
						case <-newConnection.shouldClose:
							//logging.LogDebug("got message to close connection", "server_id", newConnection.ServerID)
							p.removeConnectionsChannel <- &newConnection
							return
						case agentMessage := <-newConnection.messagesFromAgent:
							//logging.LogDebug("Writing to connection from agent", "serverID", agentMessage.ServerID)
							if dataBytes, err := base64.StdEncoding.DecodeString(agentMessage.Message); err != nil {
								logging.LogError(err, "Failed to base64 decode agent socks message", "server_id", newConnection.ServerID)
							} else if _, err := conn.Write(dataBytes); err != nil {
								logging.LogError(err, "Failed to write to connection", "server_id", newConnection.ServerID)
								p.removeConnectionsChannel <- &newConnection
								return
							} else if agentMessage.IsExit {
								//logging.LogDebug("got message isExit", "server_id", newConnection.ServerID)
								// cleanup the connection data
								p.removeConnectionsChannel <- &newConnection
								return
							}

						}
					}

				}(conn)
				go func(conn net.Conn) {
					// function for reading from Mythic's connections to send to agents
					for {
						buf := make([]byte, 4096)
						//logging.LogDebug("looping to read from connection again", "server_id", newConnection.ServerID)
						if length, err := conn.Read(buf); err != nil {
							logging.LogError(err, "Failed to read from connection, sending exit", "server_id", newConnection.ServerID)
							interceptProxyToAgentMessageChan <- interceptProxyToAgentMessage{
								Message: proxyToAgentMessage{
									Message:  nil,
									IsExit:   true,
									ServerID: newConnection.ServerID,
								},
								MessagesToAgent: p.messagesToAgent,
								CallbackID:      p.CallbackID,
								ProxyType:       p.PortType,
							}
							p.removeConnectionsChannel <- &newConnection
							return
						} else {
							if length > 0 {
								//fmt.Printf("Message received for chan %d: length %v\n", newConnection.ServerID, length)
								interceptProxyToAgentMessageChan <- interceptProxyToAgentMessage{
									Message: proxyToAgentMessage{
										Message:  buf[:length],
										IsExit:   false,
										ServerID: newConnection.ServerID,
									},
									MessagesToAgent: p.messagesToAgent,
									CallbackID:      p.CallbackID,
									ProxyType:       p.PortType,
								}
								//fmt.Printf("Message sent to p.messagesToAgent channel for chan %d\n", newConnection.ServerID)
							}
						}
					}

				}(conn)
			}
		} else {
			logging.LogError(nil, "Listener is nil, exiting the handleSocksConnections loop", "port", p.LocalPort)
			return
		}

	}
}
func (p *callbackPortUsage) handleRpfwdConnections(newConnection *acceptedConnection) {
	//logging.LogDebug("Got new connection, spinning off read and write", "server_id", newConnection.ServerID)
	go func(conn net.Conn) {
		// function for reading from agents to send to Mythic's connections
		for {
			select {
			case <-newConnection.shouldClose:
				//logging.LogDebug("got message to close connection", "server_id", newConnection.ServerID)
				p.removeConnectionsChannel <- newConnection
				return
			case agentMessage := <-newConnection.messagesFromAgent:
				//logging.LogDebug("Writing to connection from agent", "serverID", agentMessage.ServerID)
				if dataBytes, err := base64.StdEncoding.DecodeString(agentMessage.Message); err != nil {
					logging.LogError(err, "Failed to base64 decode agent socks message", "server_id", newConnection.ServerID)
				} else if _, err := conn.Write(dataBytes); err != nil {
					logging.LogError(err, "Failed to write to connection", "server_id", newConnection.ServerID)
					p.removeConnectionsChannel <- newConnection
					return
				} else if agentMessage.IsExit {
					//logging.LogDebug("got message isExit", "server_id", newConnection.ServerID)
					// cleanup the connection data
					p.removeConnectionsChannel <- newConnection
					return
				}
			}
		}
	}(newConnection.conn)
	go func(conn net.Conn) {
		// function for reading from Mythic's connections to send to agents
		for {
			buf := make([]byte, 4096)
			//logging.LogDebug("looping to read from connection", "server_id", newConnection.ServerID)
			if length, err := conn.Read(buf); err != nil {
				logging.LogError(err, "Failed to read from connection, sending exit", "server_id", newConnection.ServerID)
				interceptProxyToAgentMessageChan <- interceptProxyToAgentMessage{
					Message: proxyToAgentMessage{
						Message:  nil,
						IsExit:   true,
						ServerID: newConnection.ServerID,
					},
					MessagesToAgent: p.messagesToAgent,
					CallbackID:      p.CallbackID,
					ProxyType:       p.PortType,
				}
				p.removeConnectionsChannel <- newConnection
				return
			} else {
				if length > 0 {
					//logging.LogDebug("Message received for chan %d: length %v\n", newConnection.ServerID, length)
					interceptProxyToAgentMessageChan <- interceptProxyToAgentMessage{
						Message: proxyToAgentMessage{
							Message:  buf[:length],
							IsExit:   false,
							ServerID: newConnection.ServerID,
						},
						MessagesToAgent: p.messagesToAgent,
						ProxyType:       p.PortType,
						CallbackID:      p.CallbackID,
					}
					//fmt.Printf("Message sent to p.messagesToAgent channel for chan %d\n", newConnection.ServerID)
				}
			}
		}
	}(newConnection.conn)
}
func (p *callbackPortUsage) handleInteractiveConnections() {
	for {
		// Listen for an incoming connection
		if p.listener != nil {
			if conn, err := (*p.listener).Accept(); err != nil {
				logging.LogError(err, "Failed to accept new connection on port", "port", p.LocalPort)
				if err := (*p.listener).Close(); err != nil {
					logging.LogError(err, "Failed to close listener", "port", p.LocalPort)
				}
				//p.listener = nil
				return
			} else {
				// Handle connections in a new goroutine
				var taskUUID string
				err := database.DB.Get(&taskUUID, `SELECT agent_task_id FROM task WHERE id=$1`, p.TaskID)
				if err != nil {
					logging.LogError(err, "Failed to get task information for new interactive task connection")
					return
				}
				newConnection := acceptedConnection{
					conn:                         conn,
					shouldClose:                  make(chan bool, 1),
					interactiveMessagesFromAgent: make(chan agentMessagePostResponseInteractive, 200),
					ServerID:                     uint32(rand.Intn(math.MaxInt32)), // randomized id
					TaskUUID:                     &taskUUID,
				}
				p.newConnectionChannel <- &newConnection

				//logging.LogDebug("Got new connection", "server_id", newConnection.ServerID)
				go func(conn net.Conn) {
					// function for reading from agents to send to Mythic's connections
					for {
						select {
						case <-newConnection.shouldClose:
							//logging.LogDebug("got message to close connection", "server_id", newConnection.ServerID)
							p.removeConnectionsChannel <- &newConnection
							return
						case agentMessage := <-newConnection.interactiveMessagesFromAgent:
							//logging.LogDebug("Writing to connection from agent", "serverID", agentMessage.ServerID)
							if dataBytes, err := base64.StdEncoding.DecodeString(agentMessage.Data); err != nil {
								logging.LogError(err, "Failed to base64 decode agent interactive message", "server_id", newConnection.ServerID)
							} else if _, err := conn.Write(dataBytes); err != nil {
								logging.LogError(err, "Failed to write to connection", "server_id", newConnection.ServerID)
								p.removeConnectionsChannel <- &newConnection
								return
							} else if agentMessage.MessageType == InteractiveTask.Exit {
								//logging.LogDebug("got message isExit", "server_id", newConnection.ServerID)
								// cleanup the connection data
								p.removeConnectionsChannel <- &newConnection
								return
							}

						}
					}

				}(conn)
				go func(conn net.Conn) {
					// function for reading from Mythic's connections to send to agents
					for {
						buf := make([]byte, 4096)
						//logging.LogDebug("looping to read from connection again", "server_id", newConnection.ServerID)
						if length, err := conn.Read(buf); err != nil {
							logging.LogError(err, "Failed to read from connection, sending exit", "server_id", newConnection.ServerID)
							p.removeConnectionsChannel <- &newConnection
							return
						} else {
							if length > 0 {
								//fmt.Printf("Message received for chan %d: length %v\n", newConnection.ServerID, length)
								interceptProxyToAgentMessageChan <- interceptProxyToAgentMessage{
									InteractiveMessage: agentMessagePostResponseInteractive{
										Data:        base64.StdEncoding.EncodeToString(buf[:length]),
										TaskUUID:    taskUUID,
										MessageType: 0,
									},
									InteractiveMessagesToAgent: p.interactiveMessagesToAgent,
									CallbackID:                 p.CallbackID,
									ProxyType:                  p.PortType,
								}
								//fmt.Printf("Message sent to p.messagesToAgent channel for chan %d\n", newConnection.ServerID)
							}
						}
					}

				}(conn)
			}
		} else {
			logging.LogError(nil, "Listener is nil, exiting the handleSocksConnections loop", "port", p.LocalPort)
			return
		}

	}
}
