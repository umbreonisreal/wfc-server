package gpcm

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"wwfc/common"
	"wwfc/database"
	"wwfc/logging"
	"wwfc/qr2"

	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/logrusorgru/aurora/v3"
	"github.com/sasha-s/go-deadlock"
)

type GameSpySession struct {
	Conn                net.Conn
	WriteBuffer         string
	User                database.User
	ModuleName          string
	LoggedIn            bool
	DeviceAuthenticated bool
	Challenge           string
	AuthToken           string
	LoginTicket         string
	SessionKey          int32

	LoginInfoSet      bool
	GameName          string
	GameCode          string
	Region            byte
	Language          byte
	InGameName        string
	ConsoleFriendCode uint64
	DeviceId          uint32
	HostPlatform      string
	UnitCode          byte

	Status         string
	LocString      string
	FriendList     []uint32
	AuthFriendList []uint32

	QR2IP          uint64
	Reservation    common.MatchCommandData
	ReservationPID uint32

	NeedsExploit bool
}

var (
	ctx  = context.Background()
	pool *pgxpool.Pool
	// I would use a sync.Map instead of the map mutex combo, but this performs better.
	sessions = map[uint32]*GameSpySession{}
	mutex    = deadlock.Mutex{}

	allowDefaultDolphinKeys bool
)

func StartServer() {
	// Get config
	config := common.GetConfig()

	// Start SQL
	dbString := fmt.Sprintf("postgres://%s:%s@%s/%s", config.Username, config.Password, config.DatabaseAddress, config.DatabaseName)
	dbConf, err := pgxpool.ParseConfig(dbString)
	if err != nil {
		panic(err)
	}

	pool, err = pgxpool.ConnectConfig(ctx, dbConf)
	if err != nil {
		panic(err)
	}

	database.UpdateTables(pool, ctx)

	allowDefaultDolphinKeys = config.AllowDefaultDolphinKeys

	address := *config.GameSpyAddress + ":29900"
	l, err := net.Listen("tcp", address)
	if err != nil {
		panic(err)
	}

	// Close the listener when the application closes.
	defer l.Close()
	logging.Notice("GPCM", "Listening on", address)

	for {
		// Listen for an incoming connection.
		conn, err := l.Accept()
		if err != nil {
			panic(err)
		}

		// Handle connections in a new goroutine.
		go handleRequest(conn)
	}
}

func (g *GameSpySession) closeSession() {
	if r := recover(); r != nil {
		logging.Error(g.ModuleName, "Panic:", r)
	}

	if g.LoggedIn {
		qr2.Logout(g.User.ProfileId)
		if g.QR2IP != 0 {
			qr2.ProcessGPStatusUpdate(g.User.ProfileId, g.QR2IP, "0")
		}
		g.sendLogoutStatus()
	}

	mutex.Lock()
	defer mutex.Unlock()

	g.Conn.Close()
	if g.LoggedIn {
		g.LoggedIn = false
		delete(sessions, g.User.ProfileId)
	}
}

// Handles incoming requests.
func handleRequest(conn net.Conn) {
	session := &GameSpySession{
		Conn:           conn,
		User:           database.User{},
		ModuleName:     "GPCM:" + conn.RemoteAddr().String(),
		LoggedIn:       false,
		Challenge:      "",
		Status:         "",
		LocString:      "",
		FriendList:     []uint32{},
		AuthFriendList: []uint32{},
	}

	defer session.closeSession()

	// Set session ID and challenge
	session.Challenge = common.RandomString(10)

	err := conn.(*net.TCPConn).SetKeepAlive(true)
	if err != nil {
		logging.Error(session.ModuleName, "Unable to set keepalive:", err.Error())
	}

	payload := common.CreateGameSpyMessage(common.GameSpyCommand{
		Command:      "lc",
		CommandValue: "1",
		OtherValues: map[string]string{
			"challenge": session.Challenge,
			"id":        "1",
		},
	})
	conn.Write([]byte(payload))

	logging.Notice(session.ModuleName, "Connection established from", conn.RemoteAddr())

	// Here we go into the listening loop
	for {
		// TODO: Handle split packets
		buffer := make([]byte, 1024)
		n, err := bufio.NewReader(conn).Read(buffer)
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Client closed connection, terminate.
				logging.Info(session.ModuleName, "Client closed connection")
				return
			}

			logging.Error(session.ModuleName, "Connection lost")
			return
		}

		commands, err := common.ParseGameSpyMessage(string(buffer[:n]))
		if err != nil {
			logging.Error(session.ModuleName, "Error parsing message:", err.Error())
			logging.Error(session.ModuleName, "Raw data:", string(buffer[:n]))
			session.replyError(ErrParse)
			return
		}

		// Commands must be handled in a certain order, not in the order supplied by the client

		commands = session.handleCommand("ka", commands, func(command common.GameSpyCommand) {
			session.Conn.Write([]byte(`\ka\\final\`))
		})
		commands = session.handleCommand("login", commands, session.login)
		commands = session.handleCommand("wwfc_exlogin", commands, session.exLogin)
		commands = session.ignoreCommand("logout", commands)

		if len(commands) != 0 && session.LoggedIn == false {
			logging.Error(session.ModuleName, "Attempt to run command before login:", aurora.Cyan(commands[0]))
			session.replyError(ErrNotLoggedIn)
			return
		}

		commands = session.handleCommand("wwfc_report", commands, session.handleWWFCReport)
		commands = session.handleCommand("updatepro", commands, session.updateProfile)
		commands = session.handleCommand("status", commands, session.setStatus)
		commands = session.handleCommand("addbuddy", commands, session.addFriend)
		commands = session.handleCommand("delbuddy", commands, session.removeFriend)
		commands = session.handleCommand("authadd", commands, session.authAddFriend)
		commands = session.handleCommand("bm", commands, session.bestieMessage)
		commands = session.handleCommand("getprofile", commands, session.getProfile)

		for _, command := range commands {
			logging.Error(session.ModuleName, "Unknown command:", aurora.Cyan(command))
		}

		if session.WriteBuffer != "" {
			conn.Write([]byte(session.WriteBuffer))
			session.WriteBuffer = ""
		}
	}
}

func (g *GameSpySession) handleCommand(name string, commands []common.GameSpyCommand, handler func(command common.GameSpyCommand)) []common.GameSpyCommand {
	var unhandled []common.GameSpyCommand

	for _, command := range commands {
		if command.Command != name {
			unhandled = append(unhandled, command)
			continue
		}

		logging.Info(g.ModuleName, "Command:", aurora.Yellow(command.Command))
		handler(command)
	}

	return unhandled
}

func (g *GameSpySession) ignoreCommand(name string, commands []common.GameSpyCommand) []common.GameSpyCommand {
	var unhandled []common.GameSpyCommand

	for _, command := range commands {
		if command.Command != name {
			unhandled = append(unhandled, command)
		}
	}

	return unhandled
}
