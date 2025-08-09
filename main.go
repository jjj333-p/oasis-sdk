package oasis_sdk

import (
	"context"
	"crypto/tls"
	"encoding/xml"
	"errors"
	"fmt"

	"mellium.im/sasl"
	"mellium.im/xmpp"
	"mellium.im/xmpp/dial"
	"mellium.im/xmpp/jid"
	"mellium.im/xmpp/muc"
	"mellium.im/xmpp/mux"
	"mellium.im/xmpp/stanza"
)

// startServing is an internal function to add an internal handler to the session.
// Most of this is just obtuse things inherited from mellium
func (client *XmppClient) startServing() error {
	err := client.Session.Send(client.Ctx, stanza.Presence{Type: stanza.AvailablePresence}.Wrap(nil))
	if err != nil {
		return err
	}
	return client.Session.Serve(
		client.Multiplexer,
	)
}

// Connect dials the server and starts receiving the events.
func (client *XmppClient) Connect() error {
	d := dial.Dialer{}

	conn, err := d.DialServer(client.Ctx, "tcp", *client.JID, *client.Server)
	if err != nil {
		return errors.New("Could not connect stage 1 - " + err.Error())
	}

	client.Session, err = xmpp.NewSession(
		client.Ctx,
		client.JID.Domain(),
		*client.JID,
		conn,
		0,
		xmpp.NewNegotiator(func(*xmpp.Session, *xmpp.StreamConfig) xmpp.StreamConfig {
			return xmpp.StreamConfig{
				Lang: "en",
				Features: []xmpp.StreamFeature{
					xmpp.BindResource(),
					xmpp.StartTLS(&tls.Config{
						ServerName: *client.Server,
						MinVersion: tls.VersionTLS12,
					}),
					xmpp.SASL("", client.Login.Password, sasl.ScramSha1Plus, sasl.ScramSha1, sasl.Plain),
				},
				TeeIn:  nil,
				TeeOut: nil,
			}
		},
		))
	if err != nil {
		return errors.New("Could not connect stage 2 - " + err.Error())
	}

	if client.Session == nil {
		panic("session never got set")
	}

	//TODO: move joins elsewhere
	go func() {
		n := len(client.mucsToJoin)
		for i, mucJID := range client.mucsToJoin {
			mucStr := mucJID.Bare().String()
			fmt.Printf("Joining muc %d/%d \"%s\" with nickname \"%s\"\n", i+1, n, mucStr, mucJID.Resourcepart())
			ch, err := client.MucClient.Join(client.Ctx, mucJID, client.Session)
			if err != nil {
				println(err.Error())
				continue
			}
			client.MucChannels[mucStr] = ch
			fmt.Printf("joined muc %d/%d\n", i+1, n)
		}
	}()

	go client.DiscoServicesOnServer()

	return client.startServing()
}

// CreateClient creates the client object using the login info object and returns it
func CreateClient(
	login *LoginInfo,
	dmHandler ChatMessageHandler,
	groupMessageHandler GroupChatMessageHandler,
	chatstateHandler ChatstateHandler,
	deliveryReceiptHandler DeliveryReceiptHandler,
	readReceiptHandler ReadReceiptHandler,
) (*XmppClient, error) {

	mucJIDs := make([]jid.JID, 0, len(login.MucsToJoin))
	for _, jidStr := range login.MucsToJoin {
		//join with default displayname
		j, err := jid.Parse(jidStr + "/" + login.DisplayName)
		if err != nil {
			fmt.Println("Error parsing MUC jid: " + err.Error())
			continue
		}
		mucJIDs = append(mucJIDs, j)
	}

	// create client object
	client := &XmppClient{
		Login:                  login,
		dmHandler:              dmHandler,
		groupMessageHandler:    groupMessageHandler,
		chatstateHandler:       chatstateHandler,
		deliveryReceiptHandler: deliveryReceiptHandler,
		readReceiptHandler:     readReceiptHandler,
		mucsToJoin:             mucJIDs,
		MucChannels:            make(map[string]*muc.Channel),
	}
	client.Ctx, client.CtxCancel = context.WithCancel(context.Background())

	client.MucClient = &muc.Client{}
	messageNS := xml.Name{
		Local: "body",
	}

	// ------ chatstates -------
	composingNS := xml.Name{
		Local: "composing",
	}
	activeNS := xml.Name{
		Local: "active",
	}
	pausedNS := xml.Name{
		Local: "paused",
	}
	inactiveNS := xml.Name{
		Local: "inactive",
	}
	goneNS := xml.Name{
		Local: "gone",
	}
	// ------ chatstates ------

	// ------ receipts --------
	deliveredNS := xml.Name{
		Local: "received",
	}
	displayedNS := xml.Name{
		Local: "displayed",
	}
	// ------ receipts --------

	client.Multiplexer = mux.New(
		"jabber:client",

		//provide object to hold muc state
		muc.HandleClient(client.MucClient),

		//handlers for chat messages
		mux.MessageFunc(stanza.ChatMessage, messageNS, client.internalHandleDM),
		mux.MessageFunc(stanza.GroupChatMessage, messageNS, client.internalHandleGroupMsg),

		// Chat state handlers for direct messages
		mux.MessageFunc(stanza.ChatMessage, activeNS, client.internalActiveChatstateReceiver),
		mux.MessageFunc(stanza.ChatMessage, composingNS, client.internalComposingChatstateReciever),
		mux.MessageFunc(stanza.ChatMessage, pausedNS, client.internalPausedChatstateReceiver),
		mux.MessageFunc(stanza.ChatMessage, inactiveNS, client.internalInactiveChatstateReceiver),
		mux.MessageFunc(stanza.ChatMessage, goneNS, client.internalGoneChatstateReceiver),

		// Receipt handlers for direct messages
		mux.MessageFunc(stanza.ChatMessage, deliveredNS, client.internalHandleDeliveryReceipt),
		mux.MessageFunc(stanza.ChatMessage, displayedNS, client.internalHandleReadReceipt),

		// Chat state handlers for group messages
		mux.MessageFunc(stanza.GroupChatMessage, activeNS, client.internalActiveChatstateReceiver),
		mux.MessageFunc(stanza.GroupChatMessage, composingNS, client.internalComposingChatstateReciever),
		mux.MessageFunc(stanza.GroupChatMessage, pausedNS, client.internalPausedChatstateReceiver),
		mux.MessageFunc(stanza.GroupChatMessage, inactiveNS, client.internalInactiveChatstateReceiver),
		mux.MessageFunc(stanza.GroupChatMessage, goneNS, client.internalGoneChatstateReceiver),

		// Receipt handlers for group messages
		mux.MessageFunc(stanza.GroupChatMessage, displayedNS, client.internalHandleReadReceipt),
	)

	//string to jid object
	j, err := jid.Parse(login.User)
	if err != nil {
		return nil,
			errors.New("Could not parse user JID from `" + login.User + " - " + err.Error())
	}
	server := j.Domainpart()
	client.JID = &j
	client.Server = &server

	return client, nil
}
