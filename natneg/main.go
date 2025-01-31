package natneg

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"
	"wwfc/common"
	"wwfc/logging"

	"github.com/logrusorgru/aurora/v3"
)

const (
	NNInitRequest         = 0x00
	NNInitReply           = 0x01
	NNErtTestRequest      = 0x02
	NNErtTestReply        = 0x03
	NNStateUpdate         = 0x04
	NNConnectRequest      = 0x05
	NNConnectReply        = 0x06
	NNConnectPing         = 0x07
	NNBackupTestRequest   = 0x08
	NNBackupTestReply     = 0x09
	NNAddressCheckRequest = 0x0A
	NNAddressCheckReply   = 0x0B
	NNNatifyRequest       = 0x0C
	NNReportRequest       = 0x0D
	NNReportReply         = 0x0E
	NNPreInitRequest      = 0x0F
	NNPreInitReply        = 0x10

	// Port type
	PortTypeGamePort = 0x00
	PortTypeNATNEG1  = 0x01
	PortTypeNATNEG2  = 0x02
	PortTypeNATNEG3  = 0x03

	// NAT type
	NATTypeNoNat              = 0x00
	NATTypeFirewallOnly       = 0x01
	NATTypeFullCone           = 0x02
	NATTypeRestrictedCone     = 0x03
	NATTypePortRestrictedCone = 0x04
	NATTypeSymmetric          = 0x05
	NATTypeUnknown            = 0x06

	// NAT mapping scheme
	NATMappingUnknown           = 0x00
	NATMappingSamePrivatePublic = 0x01
	NATMappingConsistent        = 0x02
	NATMappingIncremental       = 0x03
	NATMappingMixed             = 0x04
)

type NATNEGSession struct {
	Open    bool
	Version byte
	Cookie  uint32
	Mutex   sync.RWMutex
	Clients map[byte]*NATNEGClient
}

type NATNEGClient struct {
	Cookie          uint32
	Index           byte
	ConnectingIndex byte
	ConnectAck      bool
	Connected       map[byte]bool
	NegotiateIP     string
	LocalIP         string
	ServerIP        string
	GameName        string
}

var (
	sessions   = map[uint32]*NATNEGSession{}
	mutex      = sync.RWMutex{}
	natnegConn net.PacketConn
)

func StartServer() {
	// Get config
	config := common.GetConfig()

	address := *config.GameSpyAddress + ":27901"
	conn, err := net.ListenPacket("udp", address)
	if err != nil {
		panic(err)
	}

	natnegConn = conn

	// Close the listener when the application closes.
	defer conn.Close()
	logging.Notice("NATNEG", "Listening on", address)

	for {
		buffer := make([]byte, 1024)
		size, addr, err := conn.ReadFrom(buffer)
		if err != nil {
			continue
		}

		go handleConnection(conn, addr, buffer[:size])
	}
}

func handleConnection(conn net.PacketConn, addr net.Addr, buffer []byte) {
	// Validate the packet magic
	if len(buffer) < 12 || !bytes.Equal(buffer[:6], []byte{0xfd, 0xfc, 0x1e, 0x66, 0x6a, 0xb2}) {
		logging.Error("NATNEG:"+addr.String(), "Invalid packet header")
		return
	}

	// Parse the NATNEG header
	// fd fc 1e 66 6a b2 - Packet Magic
	// xx                - Version
	// xx                - Packet Type / Command
	// xx xx xx xx       - Cookie

	version := buffer[6]
	command := buffer[7]
	cookie := binary.BigEndian.Uint32(buffer[8:12])

	moduleName := "NATNEG:" + fmt.Sprintf("%08x/", cookie) + addr.String()

	var session *NATNEGSession

	if command != NNNatifyRequest && command != NNAddressCheckRequest {
		mutex.Lock()
		var exists bool
		session, exists = sessions[cookie]
		if !exists {
			logging.Info(moduleName, "Creating session")
			session = &NATNEGSession{
				Open:    true,
				Version: version,
				Cookie:  cookie,
				Mutex:   sync.RWMutex{},
				Clients: map[byte]*NATNEGClient{},
			}
			sessions[cookie] = session

			// Session has TTL of 30 seconds
			time.AfterFunc(30*time.Second, func() {
				session.Open = false

				mutex.Lock()
				delete(sessions, cookie)
				mutex.Unlock()

				session.Mutex.Lock()
				defer session.Mutex.Unlock()

				// Disconnect each client
				for _, client := range session.Clients {
					if client.ConnectingIndex == client.Index {
						continue
					}

					logging.Info(moduleName, "Disconnecting client", aurora.Cyan(client.Index))
					// Send report ack, which will cause the client to cancel
					reportAck := createPacketHeader(version, NNReportReply, session.Cookie)
					reportAck = append(reportAck, 0x00, client.Index, 0x00)
					reportAck = append(reportAck, 0x00, 0x00, 0x00, 0x06, 0x00, 0x00)
					conn.WriteTo(reportAck, addr)
				}

				logging.Info(moduleName, "Deleted session")
			})
		}
		mutex.Unlock()

		if session.Version != version {
			logging.Error(moduleName, "Version mismatch")
			return
		}

		session.Mutex.Lock()
		defer session.Mutex.Unlock()
	}

	switch command {
	default:
		logging.Error(moduleName, "Received unknown command type:", aurora.Cyan(command))
		break

	case NNInitRequest:
		// logging.Info(moduleName, "Command:", aurora.Yellow("NN_INIT"))
		session.handleInit(conn, addr, buffer[12:], moduleName, version)
		break

	case NNInitReply:
		logging.Warn(moduleName, "Received server command:", aurora.Yellow("NN_INITACK"))
		break

	case NNErtTestRequest:
		logging.Warn(moduleName, "Received server command:", aurora.Yellow("NN_ERTTEST"))
		break

	case NNErtTestReply:
		logging.Info(moduleName, "Command:", aurora.Yellow("NN_ERTACK"))
		break

	case NNStateUpdate:
		logging.Info(moduleName, "Command:", aurora.Yellow("NN_STATEUPDATE"))
		break

	case NNConnectRequest:
		logging.Warn(moduleName, "Received server command:", aurora.Yellow("NN_CONNECT"))
		break

	case NNConnectReply:
		// logging.Info(moduleName, "Command:", aurora.Yellow("NN_CONNECT_ACK"))
		session.handleConnectReply(conn, addr, buffer[12:], moduleName, version)
		break

	case NNConnectPing:
		logging.Info(moduleName, "Command:", aurora.Yellow("NN_CONNECT_PING"))
		break

	case NNBackupTestRequest:
		logging.Info(moduleName, "Command:", aurora.Yellow("NN_BACKUP_TEST"))
		break

	case NNBackupTestReply:
		logging.Warn(moduleName, "Received server command:", aurora.Yellow("NN_BACKUP_ACK"))
		break

	case NNAddressCheckRequest:
		logging.Info(moduleName, "Command:", aurora.Yellow("NN_ADDRESS_CHECK"))
		break

	case NNAddressCheckReply:
		logging.Warn(moduleName, "Received server command:", aurora.Yellow("NN_ADDRESS_REPLY"))
		break

	case NNNatifyRequest:
		logging.Info(moduleName, "Command:", aurora.Yellow("NN_NATIFY_REQUEST"))
		break

	case NNReportRequest:
		// logging.Info(moduleName, "Command:", aurora.Yellow("NN_REPORT"))
		session.handleReport(conn, addr, buffer[12:], moduleName, version)
		break

	case NNReportReply:
		logging.Warn(moduleName, "Received server command:", aurora.Yellow("NN_REPORT_ACK"))
		break

	case NNPreInitRequest:
		logging.Info(moduleName, "Command:", aurora.Yellow("NN_PREINIT"))
		break

	case NNPreInitReply:
		logging.Warn(moduleName, "Received server command:", aurora.Yellow("NN_PREINIT_ACK"))
		break
	}
}

func getPortTypeName(portType byte) string {
	switch portType {
	default:
		return fmt.Sprintf("Unknown (0x%02x)", portType)

	case PortTypeGamePort:
		return "GamePort"

	case PortTypeNATNEG1:
		return "NATNEG1"

	case PortTypeNATNEG2:
		return "NATNEG2"

	case PortTypeNATNEG3:
		return "NATNEG3"
	}
}

func (session *NATNEGSession) handleInit(conn net.PacketConn, addr net.Addr, buffer []byte, moduleName string, version byte) {
	if len(buffer) < 10 {
		logging.Error(moduleName, "Invalid packet size")
		return
	}

	portType := buffer[0]
	clientIndex := buffer[1]
	useGamePort := buffer[2]
	localIPBytes := buffer[3:7]
	localPort := binary.BigEndian.Uint16(buffer[7:9])
	gameName, err := common.GetString(buffer[9:])
	if err != nil {
		logging.Error(moduleName, "Invalid gameName")
		return
	}

	expectedSize := 9 + len(gameName) + 1
	if len(buffer) != expectedSize {
		logging.Warn(moduleName, "Stray", aurora.BrightCyan(len(buffer)-expectedSize), "bytes after packet")
	}

	localIPStr := fmt.Sprintf("%d.%d.%d.%d:%d", localIPBytes[0], localIPBytes[1], localIPBytes[2], localIPBytes[3], localPort)

	if portType > 0x03 {
		logging.Error(moduleName, "Invalid port type")
		return
	}
	if useGamePort > 1 {
		logging.Error(moduleName, "Invalid", aurora.BrightGreen("Use Game Port"), "value")
		return
	}
	if useGamePort == 0 && portType == PortTypeGamePort {
		logging.Error(moduleName, "Request uses game port but use game port is disabled")
		return
	}

	// Write the init acknowledgement to the requester address
	ackHeader := createPacketHeader(version, NNInitReply, session.Cookie)
	ackHeader = append(ackHeader, portType, clientIndex)
	ackHeader = append(ackHeader, 0xff, 0xff, 0x6d, 0x16, 0xb5, 0x7d, 0xea)
	conn.WriteTo(ackHeader, addr)

	sender, exists := session.Clients[clientIndex]
	if !exists {
		logging.Notice(moduleName, "Creating client index", aurora.Cyan(clientIndex))

		for _, other := range session.Clients {
			if other.GameName != gameName {
				logging.Error(moduleName, "Game name mismatch", aurora.Cyan(other.GameName), "!=", aurora.Cyan(gameName))
				return
			}
		}

		sender = &NATNEGClient{
			Cookie:          session.Cookie,
			Index:           clientIndex,
			ConnectingIndex: clientIndex,
			Connected:       map[byte]bool{},
			NegotiateIP:     "",
			LocalIP:         "",
			ServerIP:        "",
			GameName:        "",
		}
		session.Clients[clientIndex] = sender
	}

	sender.GameName = gameName

	if portType != PortTypeGamePort {
		sender.NegotiateIP = addr.String()
	}
	if localPort != 0 {
		sender.LocalIP = localIPStr
	}
	if useGamePort == 0 || portType == PortTypeGamePort {
		sender.ServerIP = addr.String()
	}

	if !sender.isMapped() {
		return
	}
	// logging.Info(moduleName, "Mapped", aurora.BrightCyan(sender.NegotiateIP), aurora.BrightCyan(sender.LocalIP), aurora.BrightCyan(sender.ServerIP))

	// Send the connect requests
	session.sendConnectRequests(moduleName)
}

func (client *NATNEGClient) isMapped() bool {
	if client.NegotiateIP == "" || client.ServerIP == "" {
		return false
	}

	return true
}

func createPacketHeader(version byte, command byte, cookie uint32) []byte {
	header := []byte{0xfd, 0xfc, 0x1e, 0x66, 0x6a, 0xb2, version, command}
	return binary.BigEndian.AppendUint32(header, cookie)
}

func (session *NATNEGSession) sendConnectRequests(moduleName string) {
	for id, sender := range session.Clients {
		if !sender.isMapped() || sender.ConnectingIndex != id {
			continue
		}

		for destID, destination := range session.Clients {
			if id == destID || !destination.isMapped() || destination.ConnectingIndex != destID || destination.Connected[id] {
				continue
			}

			logging.Notice(moduleName, "Exchange connect requests between", aurora.BrightCyan(id), "and", aurora.BrightCyan(destID))
			sender.ConnectingIndex = destID
			sender.ConnectAck = false
			destination.ConnectingIndex = id
			destination.ConnectAck = false

			go func(session *NATNEGSession, sender *NATNEGClient, destination *NATNEGClient) {
				for {
					if !session.Open {
						return
					}

					check := false

					if !destination.ConnectAck && destination.ConnectingIndex == sender.Index {
						check = true
						sender.sendConnectRequestPacket(natnegConn, destination, session.Version)
					}

					if !sender.ConnectAck && sender.ConnectingIndex == destination.Index {
						check = true
						destination.sendConnectRequestPacket(natnegConn, sender, session.Version)
					}

					if !check {
						return
					}

					time.Sleep(500 * time.Millisecond)
				}
			}(session, sender, destination)
		}
	}
}

func (client *NATNEGClient) sendConnectRequestPacket(conn net.PacketConn, destination *NATNEGClient, version byte) {
	connectHeader := createPacketHeader(version, NNConnectRequest, destination.Cookie)
	connectHeader = append(connectHeader, common.IPFormatBytes(client.ServerIP)...)
	_, port := common.IPFormatToInt(client.ServerIP)
	connectHeader = binary.BigEndian.AppendUint16(connectHeader, port)
	// Two bytes: "gotyourdata" and "finished"
	connectHeader = append(connectHeader, 0x42, 0x00)

	destIPAddr, err := net.ResolveUDPAddr("udp", destination.NegotiateIP)
	if err != nil {
		panic(err)
	}
	conn.WriteTo(connectHeader, destIPAddr)
}

func (session *NATNEGSession) handleConnectReply(conn net.PacketConn, addr net.Addr, buffer []byte, moduleName string, version byte) {
	// portType := buffer[0]
	clientIndex := buffer[1]
	// useGamePort := buffer[2]
	// localIPBytes := buffer[3:7]

	if client, exists := session.Clients[clientIndex]; exists {
		client.ConnectAck = true
	}
}

func (session *NATNEGSession) handleReport(conn net.PacketConn, addr net.Addr, buffer []byte, _ string, version byte) {
	response := createPacketHeader(version, NNReportReply, session.Cookie)
	response = append(response, buffer[:9]...)
	response[14] = 0
	conn.WriteTo(response, addr)

	// portType := buffer[0]
	clientIndex := buffer[1]
	result := buffer[2]
	// natType := buffer[3]
	// mappingScheme := buffer[7]
	// gameName, err := common.GetString(buffer[11:])

	moduleName := "NATNEG:" + fmt.Sprintf("%08x/", session.Cookie) + addr.String()
	logging.Notice(moduleName, "Report from", aurora.BrightCyan(clientIndex), "result:", aurora.Cyan(result))

	if client, exists := session.Clients[clientIndex]; exists {
		client.Connected[client.ConnectingIndex] = true
		client.ConnectingIndex = clientIndex
		client.ConnectAck = false
	}

	// Send remaining requests
	session.sendConnectRequests(moduleName)
}
