package rainsd

import (
	"fmt"
	"rains/rainslib"
	"rains/utils/parser"
	"time"

	log "github.com/inconshreveable/log15"
)

//incoming messages are buffered in one of these channels until they get processed by a worker go routine
var prioChannel chan msgSectionSender
var normalChannel chan msgSectionSender
var notificationChannel chan msgSectionSender

//activeTokens contains tokens created by this server (indicate self issued queries)
//TODO create a mechanism such that this map does not grow too much in case of an attack. -> If the token is evicted before answer -> answer comes not on prio queue.
//Solution: if full do not add it to cache and these answers are then not handled with priority.???
var activeTokens = make(map[[16]byte]bool)

//capabilities contains a map with key <hash of a set of capabilities> value <[]capabilities>
var capabilities cache

//capabilities contains a map with key ConnInfo value <capabilities>
var peerToCapability cache

func initInbox() error {
	//init Channels
	prioChannel = make(chan msgSectionSender, Config.PrioBufferSize)
	normalChannel = make(chan msgSectionSender, Config.NormalBufferSize)
	notificationChannel = make(chan msgSectionSender, Config.NotificationBufferSize)

	//init Cache
	capabilities = &LRUCache{}
	err := capabilities.New(int(Config.CapabilitiesCacheSize))
	if err != nil {
		log.Error("Cannot create capabilitiesCache", "error", err)
		return err
	}
	capabilities.Add("e5365a09be554ae55b855f15264dbc837b04f5831daeb321359e18cdabab5745", []Capability{TLSOverTCP})
	capabilities.Add("76be8b528d0075f7aae98d6fa57a6d3c83ae480a8469e668d7b0af968995ac71", []Capability{NoCapability})

	peerToCapability = &LRUCache{}
	err = peerToCapability.New(int(Config.PeerToCapCacheSize))
	if err != nil {
		log.Error("Cannot create peerToCapability", "error", err)
		return err
	}

	//init parser
	msgParser = parser.RainsMsgParser{}

	createWorker()

	//TODO CFE for testing purposes (afterwards remove)
	addToActiveTokenCache("456")
	return nil
}

//addToActiveTokenCache adds tok to the active tocken cache
func addToActiveTokenCache(tok string) {
	if len(tok) > 16 {
		tok = tok[:16]
	}
	var token [16]byte
	copy(token[0:len(tok)], []byte(tok))
	activeTokens[token] = true
}

//deliver pushes all incoming messages to the prio or normal channel based on some strategy
func deliver(message []byte, sender ConnInfo) {
	if uint(len(message)) > Config.MaxMsgByteLength {
		token, _ := msgParser.Token(message)
		sendNotificationMsg(token, sender, rainslib.MsgTooLarge)
		return
	}
	msg, err := msgParser.ParseByteSlice(message)
	if err != nil {
		sendNotificationMsg(msg.Token, sender, rainslib.BadMessage)
		return
	}
	log.Info("Parsed Message", "msg", msg)
	//TODO CFE this part must be refactored once we have a CBOR parser so we can distinguish an array of capabilities from a sha hash entry
	//TODO send caps not understood back to sender if hash not in cash
	if msg.Capabilities != "" {
		if caps, ok := capabilities.Get(msg.Capabilities); ok {
			peerToCapability.Add(sender, caps)
		} else {
			switch Capability(msg.Capabilities) {
			case TLSOverTCP:
				peerToCapability.Add(sender, TLSOverTCP)
			case NoCapability:
				peerToCapability.Add(sender, NoCapability)
			default:
				log.Warn("Sent capability value does not match know capability", "rcvCaps", msg.Capabilities)
			}
		}
	}
	if !verifyMessageSignature(msg.Signatures) {
		return
	}
	for _, m := range msg.Content {
		switch m := m.(type) {
		case *rainslib.AssertionSection, *rainslib.ShardSection, *rainslib.ZoneSection:
			addMsgSectionToQueue(m, msg.Token, sender)
		case *rainslib.QuerySection:
			addQueryToQueue(m, msg, sender)
		case *rainslib.NotificationSection:
			addNotificationToQueue(m, msg.Token, sender)
		default:
			log.Warn(fmt.Sprintf("unsupported message section type %T", m))
		}
	}
}

//sendNotificationMsg sends a notification message to the sender with the given notificationType. If an error occurs during parsing no message is sent and the error is logged.
func sendNotificationMsg(token rainslib.Token, sender ConnInfo, notificationType rainslib.NotificationType) {
	msg, err := CreateNotificationMsg(token, notificationType, "")
	if err != nil {
		log.Warn("Cannot send notification error due to parser error", "error", err)
		return
	}
	sendTo(msg, sender)
}

//addMsgSectionToQueue looks up the token of the msg in the activeTokens cache and if present adds the msg section to the prio cache, otherwise to the normal cache.
func addMsgSectionToQueue(msgSection rainslib.MessageSection, tok rainslib.Token, sender ConnInfo) {
	if _, ok := activeTokens[tok]; ok {
		log.Info("active Token encountered", "token", tok)
		prioChannel <- msgSectionSender{Sender: sender, Msg: msgSection, Token: tok}
	} else {
		log.Info("token not in active token cache", "token", tok)
		normalChannel <- msgSectionSender{Sender: sender, Msg: msgSection, Token: tok}
	}
}

//addQueryToQueue checks that the token of the message and of the query section are the same and if so adds it to a queue
func addQueryToQueue(section *rainslib.QuerySection, msg rainslib.RainsMessage, sender ConnInfo) {
	if msg.Token == section.Token {
		normalChannel <- msgSectionSender{Sender: sender, Msg: section, Token: msg.Token}
	} else {
		log.Warn("Token of message and query section do not match.", "msgToken", msg.Token, "querySectionToken", section.Token)
		sendNotificationMsg(msg.Token, sender, rainslib.BadMessage)
		sendNotificationMsg(section.Token, sender, rainslib.BadMessage)
	}
}

//addNotificationToQueue adds a rains message containing one notification message section to the queue if the token is present in the activeToken cache
func addNotificationToQueue(msg *rainslib.NotificationSection, tok rainslib.Token, sender ConnInfo) {
	if _, ok := activeTokens[tok]; ok {
		log.Info("active Token encountered", "token", tok)
		delete(activeTokens, tok)
		notificationChannel <- msgSectionSender{Sender: sender, Msg: msg, Token: tok}
	} else {
		log.Warn("Token not in active token cache, drop message", "token", tok)
	}
}

//createWorker creates go routines which process messages from the prioChannel and normalChannel.
//number of go routines per queue are loaded from the config
func createWorker() {
	for i := 0; i < int(Config.PrioWorkerCount); i++ {
		go workPrio()
	}
	for i := 0; i < int(Config.NormalWorkerCount); i++ {
		go workBoth()
	}
	for i := 0; i < int(Config.NotificationWorkerCount); i++ {
		go workNotification()
	}
	//TODO CFE load number from config
	for i := 0; i < 2; i++ {
		go workPendingSignatures()
	}
}

//workBoth works on the prioChannel and on the normalChannel. A worker only fetches a message from the normalChannel if the prioChannel is empty
func workBoth() {
	for {
		select {
		case msg := <-prioChannel:
			verify(msg)
		case msg := <-normalChannel:
			verify(msg)
		default:
			//TODO CFE add to config?
			time.Sleep(50 * time.Millisecond)
		}
	}
}

//workPrio only works on prioChannel. This is necessary to avoid deadlock
func workPrio() {
	for {
		select {
		case msg := <-prioChannel:
			verify(msg)
		default:
			//TODO CFE add to config?
			time.Sleep(50 * time.Millisecond)
		}
	}
}

//workNotification works on notificationChannel.
func workNotification() {
	for {
		select {
		case msg := <-notificationChannel:
			notify(msg)
		default:
			//TODO CFE add to config?
			time.Sleep(50 * time.Millisecond)
		}
	}
}