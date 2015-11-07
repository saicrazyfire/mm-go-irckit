package irckit

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jpillora/backoff"
	"github.com/mattermost/platform/model"
	"github.com/sorcix/irc"
)

func NewUserMM(c net.Conn, srv Server) *User {
	u := NewUser(&conn{
		Conn:    c,
		Encoder: irc.NewEncoder(c),
		Decoder: irc.NewDecoder(c),
	})
	u.Srv = srv

	// used for login
	mattermostService := &User{Nick: "mattermost", User: "mattermost", Real: "ghost", Host: "abchost", channels: map[Channel]struct{}{}}
	mattermostService.MmGhostUser = true
	srv.Add(mattermostService)
	if _, ok := srv.HasUser("mattermost"); !ok {
		go srv.Handle(mattermostService)
	}

	return u
}

func (u *User) loginToMattermost() error {
	b := &backoff.Backoff{
		Min:    time.Second,
		Max:    5 * time.Minute,
		Jitter: true,
	}
	// login to mattermost
	//u.Credentials = &MmCredentials{Server: url, Team: team, Login: email, Pass: pass}
	MmClient := model.NewClient("https://" + u.Credentials.Server)
	var myinfo *model.Result
	var appErr *model.AppError
	for {
		logger.Debug("retrying login", u.Credentials.Team, u.Credentials.Login, u.Credentials.Server)
		myinfo, appErr = MmClient.LoginByEmail(u.Credentials.Team, u.Credentials.Login, u.Credentials.Pass)
		if appErr != nil {
			d := b.Duration()
			logger.Infof("LOGIN: %s, reconnecting in %s", appErr, d)
			time.Sleep(d)
			continue
		}
		break
	}
	// reset timer
	b.Reset()
	u.MmUser = myinfo.Data.(*model.User)

	myinfo, _ = MmClient.GetMyTeam("")
	u.MmTeam = myinfo.Data.(*model.Team)

	// setup websocket connection
	wsurl := "wss://" + u.Credentials.Server + "/api/v1/websocket"
	header := http.Header{}
	header.Set(model.HEADER_AUTH, "BEARER "+MmClient.AuthToken)

	var WsClient *websocket.Conn
	var err error
	for {
		WsClient, _, err = websocket.DefaultDialer.Dial(wsurl, header)
		if err != nil {
			d := b.Duration()
			logger.Infof("WSS: %s, reconnecting in %s", err, d)
			time.Sleep(d)
			continue
		}
		break
	}
	b.Reset()

	newserver := NewServer("matterircd")
	newserver.Add(u)
	go newserver.Handle(u)
	u.Srv = newserver

	u.MmClient = MmClient
	u.MmWsClient = WsClient

	// populating users
	mmusers, _ := u.MmClient.GetProfiles(u.MmUser.TeamId, "")
	u.MmUsers = mmusers.Data.(map[string]*model.User)

	// populating channels
	mmchannels, _ := MmClient.GetChannels("")
	u.MmChannels = mmchannels.Data.(*model.ChannelList)

	mmchannels, _ = MmClient.GetMoreChannels("")
	u.MmMoreChannels = mmchannels.Data.(*model.ChannelList)

	// fetch users and channels from mattermost
	u.addUsersToChannels()

	return nil
}

func (u *User) createMMUser(nick string, user string) *User {
	if ghost, ok := u.Srv.HasUser(nick); ok {
		return ghost
	}
	ghost := &User{Nick: nick, User: user, Real: "ghost", Host: u.MmClient.Url, channels: map[Channel]struct{}{}}
	ghost.MmGhostUser = true
	return ghost
}

func (u *User) addUsersToChannels() {
	var mmConnected bool
	srv := u.Srv
	// already connected to a mm server ? add teamname as suffix
	if _, ok := srv.HasChannel("#town-square"); ok {
		//mmConnected = true
	}
	rate := time.Second / 1
	throttle := time.Tick(rate)

	for _, mmchannel := range u.MmChannels.Channels {

		// exclude direct messages
		if strings.Contains(mmchannel.Name, "__") {
			continue
		}
		<-throttle
		go func(mmchannel *model.Channel) {
			edata, _ := u.MmClient.GetChannelExtraInfo(mmchannel.Id, "")
			if mmConnected {
				mmchannel.Name = mmchannel.Name + "-" + u.MmTeam.Name
			}

			// join ourself to all channels
			ch := srv.Channel("#" + mmchannel.Name)
			ch.Join(u)

			// add everyone on the MM channel to the IRC channel
			for _, d := range edata.Data.(*model.ChannelExtra).Members {
				if mmConnected {
					d.Username = d.Username + "-" + u.MmTeam.Name
				}
				// already joined
				if d.Id == u.MmUser.Id {
					continue
				}

				cghost, ok := srv.HasUser(d.Username)
				if !ok {
					ghost := &User{Nick: d.Username, User: d.Id,
						Real: "ghost", Host: u.MmClient.Url, channels: map[Channel]struct{}{}}

					ghost.MmGhostUser = true
					logger.Info("adding", ghost.Nick, "to #"+mmchannel.Name)
					srv.Add(ghost)
					go srv.Handle(ghost)
					ch := srv.Channel("#" + mmchannel.Name)
					ch.Join(ghost)
				} else {
					ch := srv.Channel("#" + mmchannel.Name)
					ch.Join(cghost)
				}
			}

			// post everything to the channel you haven't seen yet
			postlist := u.getMMPostsSince(mmchannel.Id, u.MmChannels.Members[mmchannel.Id].LastViewedAt)
			if postlist == nil {
				logger.Errorf("something wrong with getMMPostsSince")
				return
			}
			logger.Debugf("%#v", u.MmChannels.Members[mmchannel.Id])
			for _, id := range postlist.Order {
				for _, post := range strings.Split(postlist.Posts[id].Message, "\n") {
					ch.SpoofMessage(u.MmUsers[postlist.Posts[id].UserId].Username, post)
				}
			}
			u.updateMMLastViewed(mmchannel.Id)

		}(mmchannel)
	}

	// add all users, also who are not on channels
	for _, mmuser := range u.MmUsers {
		// do not add our own nick
		if mmuser.Id == u.MmUser.Id {
			continue
		}
		_, ok := srv.HasUser(mmuser.Username)
		if !ok {
			if mmConnected {
				mmuser.Username = mmuser.Username + "-" + u.MmTeam.Name
			}
			ghost := &User{Nick: mmuser.Username, User: mmuser.Id,
				Real: "ghost", Host: u.MmClient.Url, channels: map[Channel]struct{}{}}
			ghost.MmGhostUser = true
			logger.Info("adding", ghost.Nick, "without a channel")
			srv.Add(ghost)
			go srv.Handle(ghost)
		}
	}
}

type MmInfo struct {
	MmGhostUser    bool
	MmClient       *model.Client
	MmWsClient     *websocket.Conn
	Srv            Server
	MmUsers        map[string]*model.User
	MmUser         *model.User
	MmChannels     *model.ChannelList
	MmMoreChannels *model.ChannelList
	MmTeam         *model.Team
	Credentials    *MmCredentials
}

type MmCredentials struct {
	Login  string
	Team   string
	Pass   string
	Server string
}

func (u *User) WsReceiver() {
	var rmsg model.Message
	for {
		if err := u.MmWsClient.ReadJSON(&rmsg); err != nil {
			logger.Critical(err)
			// reconnect
			u.loginToMattermost()
		}
		logger.Debugf("WsReceiver: %#v", rmsg)
		if rmsg.Action == model.ACTION_POSTED {
			data := model.PostFromJson(strings.NewReader(rmsg.Props["post"]))
			logger.Debug("receiving userid", data.UserId)
			if data.UserId == u.MmUser.Id {
				// our own message
				continue
			}
			// we don't have the user, refresh the userlist
			if u.MmUsers[data.UserId] == nil {
				mmusers, _ := u.MmClient.GetProfiles(u.MmUser.TeamId, "")
				u.MmUsers = mmusers.Data.(map[string]*model.User)
			}
			ghost := u.createMMUser(u.MmUsers[data.UserId].Username, data.UserId)
			rcvchannel := u.getMMChannelName(data.ChannelId)
			if strings.Contains(rcvchannel, "__") {
				var rcvuser string
				rcvusers := strings.Split(rcvchannel, "__")
				if rcvusers[0] != u.MmUser.Id {
					rcvuser = u.MmUsers[rcvusers[0]].Username
				} else {
					rcvuser = u.MmUsers[rcvusers[1]].Username
				}

				u.Encode(&irc.Message{
					Prefix:   &irc.Prefix{Name: rcvuser, User: rcvuser, Host: rcvuser},
					Command:  irc.PRIVMSG,
					Params:   []string{u.Nick},
					Trailing: data.Message,
				})
				//u.Srv.Publish(&event{UserMsgEvent, u.Srv, nil, u, msg})
				continue
			}

			logger.Debugf("channel id %#v, name %#v", data.ChannelId, u.getMMChannelName(data.ChannelId))
			ch := u.Srv.Channel("#" + u.getMMChannelName(data.ChannelId))
			ch.Join(ghost)
			msgs := strings.Split(data.Message, "\n")
			for _, m := range msgs {
				ch.Message(ghost, m)
			}
			//ch := srv.Channel("#" + data.Channel)

			//mychan[0].Message(ghost, data.Message)
			logger.Debug(u.MmUsers[data.UserId].Username, ":", data.Message)
			logger.Debugf("%#v", data)

			// updatelastviewed
			u.updateMMLastViewed(data.ChannelId)
		}
		if rmsg.Action == model.ACTION_USER_REMOVED {
			if u.MmUsers[rmsg.UserId] == nil {
				mmusers, _ := u.MmClient.GetProfiles(u.MmUser.TeamId, "")
				u.MmUsers = mmusers.Data.(map[string]*model.User)
			}
			// remove ourselves from the channel
			if rmsg.UserId == u.MmUser.Id {
				ch := u.Srv.Channel("#" + u.getMMChannelName(rmsg.ChannelId))
				ch.Part(u, "")
				continue
			}

			ghost := u.createMMUser(u.MmUsers[rmsg.UserId].Username, rmsg.UserId)
			if ghost == nil {
				logger.Debug("couldn't remove user", rmsg.UserId, u.MmUsers[rmsg.UserId].Username)
				continue
			}
			ch := u.Srv.Channel("#" + u.getMMChannelName(rmsg.ChannelId))
			ch.Part(ghost, "")
		}
		if rmsg.Action == model.ACTION_USER_ADDED {
			if u.getMMChannelName(rmsg.ChannelId) == "" {
				u.updateMMChannels()
			}

			if u.MmUsers[rmsg.UserId] == nil {
				mmusers, _ := u.MmClient.GetProfiles(u.MmUser.TeamId, "")
				u.MmUsers = mmusers.Data.(map[string]*model.User)
			}
			// add ourselves to the channel
			if rmsg.UserId == u.MmUser.Id {
				ch := u.Srv.Channel("#" + u.getMMChannelName(rmsg.ChannelId))
				logger.Debug("ACTION_USER_ADDED adding myself to", u.getMMChannelName(rmsg.ChannelId), rmsg.ChannelId)
				ch.Join(u)
				continue
			}
			ghost := u.createMMUser(u.MmUsers[rmsg.UserId].Username, rmsg.UserId)
			if ghost == nil {
				logger.Debug("couldn't add user", rmsg.UserId, u.MmUsers[rmsg.UserId].Username)
				continue
			}
			ch := u.Srv.Channel("#" + u.getMMChannelName(rmsg.ChannelId))
			ch.Join(ghost)
		}
	}
}

func (u *User) getMMChannelName(id string) string {
	for _, channel := range append(u.MmChannels.Channels, u.MmMoreChannels.Channels...) {
		if channel.Id == id {
			return channel.Name
		}
	}
	return ""
}

func (u *User) getMMChannelId(name string) string {
	for _, channel := range append(u.MmChannels.Channels, u.MmMoreChannels.Channels...) {
		if channel.Name == name {
			return channel.Id
		}
	}
	return ""
}

func (u *User) getMMUserId(name string) string {
	for id, u := range u.MmUsers {
		if u.Username == name {
			return id
		}
	}
	return ""
}

func (u *User) MsgUser(toUser *User, msg string) {
	u.Encode(&irc.Message{
		Prefix:   toUser.Prefix(),
		Command:  irc.PRIVMSG,
		Params:   []string{u.Nick},
		Trailing: msg,
	})
}

func (u *User) handleMMDM(toUser *User, msg string) {
	var channel string
	// We don't have a DM with this user yet.
	if u.getMMChannelId(toUser.User+"__"+u.MmUser.Id) == "" && u.getMMChannelId(u.MmUser.Id+"__"+toUser.User) == "" {
		// create DM channel
		_, err := u.MmClient.CreateDirectChannel(map[string]string{"user_id": toUser.User})
		if err != nil {
			logger.Debugf("direct message to %#v failed: %s", toUser, err)
		}
		// update our channels
		mmchannels, _ := u.MmClient.GetChannels("")
		u.MmChannels = mmchannels.Data.(*model.ChannelList)
	}

	// build the channel name
	if toUser.User > u.MmUser.Id {
		channel = u.MmUser.Id + "__" + toUser.User
	} else {
		channel = toUser.User + "__" + u.MmUser.Id
	}
	// build & send the message
	post := &model.Post{ChannelId: u.getMMChannelId(channel), Message: msg}
	u.MmClient.CreatePost(post)
}

func (u *User) handleMMServiceBot(toUser *User, msg string) {
	commands := strings.Fields(msg)
	switch commands[0] {
	case "LOGIN":
		{
			data := strings.Split(msg, " ")
			if len(data) != 5 {
				u.MsgUser(toUser, "need LOGIN <server> <team> <login> <pass>")
				return
			}
			u.Credentials = &MmCredentials{Server: data[1], Team: data[2], Login: data[3], Pass: data[4]}
			//err := u.loginToMattermost(data[1], data[2], data[3], data[4])
			err := u.loginToMattermost()
			if err != nil {
				u.MsgUser(toUser, "login failed")
				return
			}
			go u.WsReceiver()
			u.MsgUser(toUser, "login OK")

		}
	default:
		u.MsgUser(toUser, "need LOGIN <server> <team> <login> <pass>")
	}

}

func (u *User) syncMMChannel(id string, name string) {
	var mmConnected bool
	srv := u.Srv
	// already connected to a mm server ? add teamname as suffix
	if _, ok := srv.HasChannel("#town-square"); ok {
		//mmConnected = true
	}

	edata, _ := u.MmClient.GetChannelExtraInfo(id, "")
	for _, d := range edata.Data.(*model.ChannelExtra).Members {
		if mmConnected {
			d.Username = d.Username + "-" + u.MmTeam.Name
		}
		// join all the channels we're on on MM
		if d.Id == u.MmUser.Id {
			ch := srv.Channel("#" + name)
			logger.Debug("syncMMChannel adding myself to ", name, id)
			ch.Join(u)
		}

		cghost, ok := srv.HasUser(d.Username)
		if !ok {
			ghost := &User{Nick: d.Username, User: d.Id,
				Real: "ghost", Host: u.MmClient.Url, channels: map[Channel]struct{}{}}

			ghost.MmGhostUser = true
			logger.Info("adding", ghost.Nick, "to #"+name)
			srv.Add(ghost)
			go srv.Handle(ghost)
			ch := srv.Channel("#" + name)
			ch.Join(ghost)
		} else {
			ch := srv.Channel("#" + name)
			ch.Join(cghost)
		}
	}
}

func (u *User) joinMMChannel(channel string) error {
	if u.getMMChannelId(strings.Replace(channel, "#", "", 1)) == "" {
		return errors.New("failed to join")
	}
	_, err := u.MmClient.JoinChannel(u.getMMChannelId(strings.Replace(channel, "#", "", 1)))
	if err != nil {
		return errors.New("failed to join")
	}
	u.syncMMChannel(u.getMMChannelId(strings.Replace(channel, "#", "", 1)), strings.Replace(channel, "#", "", 1))
	return nil
}

func (u *User) getMMPostsSince(channelId string, time int64) *model.PostList {
	res, err := u.MmClient.GetPostsSince(channelId, time)
	if err != nil {
		return nil
	}
	return res.Data.(*model.PostList)
}

func (u *User) updateMMLastViewed(channelId string) {
	logger.Debugf("posting lastview %#v", channelId)
	_, err := u.MmClient.UpdateLastViewedAt(channelId)
	if err != nil {
		logger.Info(err)
	}
}

func (u *User) updateMMChannels() error {
	mmchannels, _ := u.MmClient.GetChannels("")
	u.MmChannels = mmchannels.Data.(*model.ChannelList)
	mmchannels, _ = u.MmClient.GetMoreChannels("")
	u.MmMoreChannels = mmchannels.Data.(*model.ChannelList)
	return nil
}
