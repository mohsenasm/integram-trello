package trello

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/mgo.v2/bson"

	iurl "github.com/requilence/url"

	log "github.com/sirupsen/logrus"

	"github.com/jinzhu/now"
	"github.com/mohsenasm/integram"
	t "github.com/mohsenasm/integram-trello/api"
	"github.com/mrjones/oauth"
	"github.com/requilence/decent"
	tg "github.com/requilence/telegram-bot-api"
)

var m = integram.HTMLRichText{}

// Config contains OAuthProvider
type Config struct {
	integram.OAuthProvider
	integram.BotConfig
}

var defaultBoardFilter = ChatBoardFilterSettings{CardCreated: true, CardCommented: true, CardMoved: true, PersonAssigned: true, Archived: true, Due: true}

const (
	markSign          = "✅ "
	dueDateFormat     = "02.01 15:04"
	dueDateFullFormat = "02.01.2006 15:04"
)

const (
	cardMemberStateUnassigned = 0
	cardMemberStateAssigned   = 1
)

const (
	cardLabelStateUnattached = 0
	cardLabelStateAttached   = 1
)

// Service returns *integram.Service from trello.Config
func (cfg Config) Service() *integram.Service {
	return &integram.Service{
		Name:        "trello",
		NameToPrint: "Trello",
		DefaultOAuth1: &integram.DefaultOAuth1{
			Key:    cfg.OAuthProvider.ID,
			Secret: cfg.OAuthProvider.Secret,

			RequestTokenURL:   "https://trello.com/1/OAuthGetRequestToken",
			AuthorizeTokenURL: "https://trello.com/1/OAuthAuthorizeToken",
			AccessTokenURL:    "https://trello.com/1/OAuthGetAccessToken",

			AdditionalAuthorizationURLParams: map[string]string{
				"name":       "Integram",
				"expiration": "never",
				"scope":      "read,write",
			},

			AccessTokenReceiver: accessTokenReceiver,
		},

		JobsPool: 10,
		Jobs: []integram.Job{
			{sendBoardsToIntegrate, 10, integram.JobRetryFibonacci},
			{sendBoardsForCard, 10, integram.JobRetryFibonacci},
			{subscribeBoard, 10, integram.JobRetryFibonacci},
			{cacheAllCards, 1, integram.JobRetryFibonacci},
			{commentCard, 10, integram.JobRetryFibonacci},
			{downloadAttachment, 10, integram.JobRetryFibonacci},
			{removeFile, 1, integram.JobRetryFibonacci},
			{attachFileToCard, 3, integram.JobRetryFibonacci},
			{resubscribeAllBoards, 1, integram.JobRetryFibonacci},
		},
		Actions: []interface{}{
			boardToIntegrateSelected,
			boardForCardSelected,
			listForCardSelected,
			textForCardEntered,
			targetChatSelected,
			cardReplied,
			commentCard,
			attachFileToCard,
			afterBoardIntegratedActionSelected,
			//			afterCardCreatedActionSelected,
			sendBoardFiltersKeyboard,
			boardFilterButtonPressed,
			сardDueDateEntered,
			inlineCardButtonPressed,
			сardDescEntered,
			сardNameEntered,
		},
		TGNewMessageHandler:         newMessageHandler,
		TGInlineQueryHandler:        inlineQueryHandler,
		TGChosenInlineResultHandler: chosenInlineResultHandler,
		WebhookHandler:              webhookHandler,
		OAuthSuccessful:             oAuthSuccessful,
	}
}

// ChatSettings contains filters information
type ChatSettings struct {
	Boards map[string]ChatBoardSetting
}

// UserSettings contains boards data and target chats to deliver notifications
type UserSettings struct {
	// Boards settings by ID
	Boards map[string]UserBoardSetting
	// Chat from which integration request is received
	TargetChat *integram.Chat
}

// ChatBoardFilterSettings customize which events will produce messages
type ChatBoardFilterSettings struct {
	CardCreated    bool
	CardCommented  bool
	CardMoved      bool
	PersonAssigned bool
	Labeled        bool
	Voted          bool
	Archived       bool
	Checklisted    bool
	Due            bool
}

// ChatBoardSetting contains Trello board settings
type ChatBoardSetting struct {
	Name            string // Board name
	Enabled         bool   // Enable notifications on that board
	Filter          ChatBoardFilterSettings
	OAuthToken      string // backward compatibility for some of migrated from v1 users
	TrelloWebhookID string // backward compatibility for some of migrated from v1 users
	User            int64  // ID of User who integrate this board into this Chat
}

// UserBoardSetting contains Trello board settings
type UserBoardSetting struct {
	Name            string // Board name
	TrelloWebhookID string // Trello Webhook id
	OAuthToken      string // To avoid stuck webhook when OAUthToken was changed. Because Webhook relates to token, not to App
}

func userSettings(c *integram.Context) UserSettings {
	s := UserSettings{}
	c.User.Settings(&s)
	return s
}

func chatSettings(c *integram.Context) ChatSettings {
	s := ChatSettings{}
	c.Chat.Settings(&s)
	return s
}

type webhookInfo struct {
	ID          string
	DateCreated time.Time
	DateExpires time.Time
	IDMember    string
	IDModel     string
	CallbackURL string
}

func oAuthSuccessful(c *integram.Context) error {
	var err error
	b := false
	if c.User.Cache("auth_redirect", &b) {
		err = c.NewMessage().SetText("Great! Now you can use reply-to-comment and inline buttons 🙌 You can return to your group").Send()
	} else {
		_, err = c.Service().DoJob(sendBoardsToIntegrate, c)
	}
	if err != nil {
		return err
	}
	err = resubscribeAllBoards(c)

	return err
}

func accessTokenReceiver(c *integram.Context, r *http.Request, requestToken *oauth.RequestToken) (token string, err error) {
	values := r.URL.Query()
	verificationCode := values.Get("oauth_verifier")

	accessToken, err := c.OAuthProvider().OAuth1Client(c).AuthorizeToken(requestToken, verificationCode)
	if err != nil || accessToken == nil {
		c.Log().Error(err)
		return "", err
	}

	return accessToken.Token, err
}

func api(c *integram.Context) *t.Client {
	token := c.User.OAuthToken()

	if token == "" {
		cs := chatSettings(c)
		// todo: bad workaround to handle some chats from v1
		if len(cs.Boards) > 0 {
			for _, board := range cs.Boards {
				if board.User == 0 && board.OAuthToken != "" {
					token = board.OAuthToken
				}
			}
		}
	}

	return t.New(c.Service().DefaultOAuth1.Key, c.Service().DefaultOAuth1.Secret, token)
}

func me(c *integram.Context, api *t.Client) (*t.Member, error) {
	me := &t.Member{}
	if exists := c.User.Cache("me", me); exists {
		return me, nil
	}
	var err error
	me, err = api.Member("me")

	if t.IsBadToken(err) {
		c.User.ResetOAuthToken()
	}

	if err != nil {
		c.Log().WithError(err).Error("Can't get me member")
		return nil, err
	}
	c.User.SetCache("me", me, time.Hour)
	c.SetServiceCache("nick_map_"+me.Username, c.User.UserName, time.Hour*24*365)

	return me, nil
}

func getBoardData(c *integram.Context, api *t.Client, boardID string) ([]*t.List, []*t.Member, []*t.Label, error) {
	var boardData struct {
		Lists   []*t.List
		Members []*t.Member
		Labels  []*t.Label
	}

	b, err := api.Request("GET", "boards/"+boardID, nil, url.Values{"lists": {"open"}, "lists_fields": {"name"}, "members": {"all"}, "labels": {"all"}, "member_fields": {"fullName,username"}})

	if t.IsBadToken(err) {
		c.User.ResetOAuthToken()
	}

	if err != nil {
		return nil, nil, nil, err
	}

	err = json.Unmarshal(b, &boardData)

	if err != nil {
		c.Log().WithField("id", boardID).WithError(err).Error("Can't get board lists")
		return nil, nil, nil, err
	}
	err = c.SetServiceCache("lists_"+boardID, boardData.Lists, time.Hour*6)
	err = c.SetServiceCache("members_"+boardID, boardData.Members, time.Hour*24*7)
	err = c.SetServiceCache("labels_"+boardID, boardData.Labels, time.Hour*24*7)

	if err != nil {
		c.Log().WithError(err).Error("Can't save to cache")
		return nil, nil, nil, err
	}
	return boardData.Lists, boardData.Members, boardData.Labels, nil
}

func listsByBoardID(c *integram.Context, api *t.Client, boardID string) ([]*t.List, error) {
	var lists []*t.List

	if exists := c.ServiceCache("lists_"+boardID, &lists); exists {
		return lists, nil
	}

	var err error
	lists, _, _, err = getBoardData(c, api, boardID)

	if err != nil {
		return nil, err
	}

	return lists, nil
}

func labelsByBoardID(c *integram.Context, api *t.Client, boardID string) ([]*t.Label, error) {
	var labels []*t.Label

	if exists := c.ServiceCache("labels_"+boardID, &labels); exists {
		return labels, nil
	}

	var err error
	_, _, labels, err = getBoardData(c, api, boardID)

	if err != nil {
		return nil, err
	}

	sort.Sort(byActuality(labels))
	return labels, nil
}

func membersByBoardID(c *integram.Context, api *t.Client, boardID string) ([]*t.Member, error) {
	var members []*t.Member

	if exists := c.ServiceCache("members_"+boardID, &members); exists {
		return members, nil
	}

	var err error
	_, members, _, err = getBoardData(c, api, boardID)

	if err != nil {
		return nil, err
	}

	return members, nil
}

func boardsMaps(boards []*t.Board) map[string]*t.Board {
	m := make(map[string]*t.Board)
	for _, board := range boards {
		m[board.Id] = board
	}
	return m
}

func boards(c *integram.Context, api *t.Client) ([]*t.Board, error) {
	var boards []*t.Board

	if exists := c.User.Cache("boards", &boards); exists {
		return boards, nil
	}

	var err error
	b, err := api.Request("GET", "members/me/boards", nil, url.Values{"filter": {"open"}})
	if t.IsBadToken(err) {
		c.User.ResetOAuthToken()
	}

	if err != nil {
		return nil, err
	}

	err = json.Unmarshal(b, &boards)

	if err != nil {
		c.Log().WithError(err).Error("Can't get my boards")
		return nil, err
	}
	err = c.User.SetCache("boards", boards, time.Hour)

	if err != nil {
		c.Log().WithError(err).Error("Can't save to cache")
		return nil, err
	}

	return boards, nil
}

type byPriority struct {
	Cards []*t.Card
	MeID  string
}

func (a byPriority) Len() int {
	return len(a.Cards)
}

func (a byPriority) Swap(i, j int) {
	a.Cards[i], a.Cards[j] = a.Cards[j], a.Cards[i]
}

func (a byPriority) DueBefore(i, j int) bool {
	return (a.Cards[i].Due != nil && !a.Cards[i].Due.IsZero() && (a.Cards[j].Due == nil || a.Cards[j].Due.IsZero() || a.Cards[i].Due.Before(*(a.Cards[j].Due))))
}

func (a byPriority) Assigned(i, j int) bool {
	if len(a.Cards[i].IdMembers) > 0 && integram.SliceContainsString(a.Cards[i].IdMembers, a.MeID) && (len(a.Cards[j].IdMembers) == 0 || !integram.SliceContainsString(a.Cards[j].IdMembers, a.MeID)) {
		return true
	}

	return false
}

func (a byPriority) VotesMore(i, j int) bool {
	return (len(a.Cards[i].IdMembersVoted) > len(a.Cards[j].IdMembersVoted))
}

func (a byPriority) PosLess(i, j int) bool {
	return a.Cards[i].IdList == a.Cards[j].IdList && (a.Cards[i].Pos < a.Cards[j].Pos)
}

func (a byPriority) LastActivityOlder(i, j int) bool {
	return (a.Cards[i].DateLastActivity != nil && !a.Cards[i].DateLastActivity.IsZero() && (a.Cards[j].DateLastActivity == nil || a.Cards[j].DateLastActivity.IsZero() || a.Cards[i].DateLastActivity.Before(*(a.Cards[j].DateLastActivity))))
}

func (a byPriority) LastBorderActivityMoreRecent(i, j int) bool {
	return a.Cards[i].Board != nil && a.Cards[j].Board != nil && (a.Cards[i].Board.DateLastActivity != nil && !a.Cards[i].Board.DateLastActivity.IsZero() && (a.Cards[j].Board.DateLastActivity != nil && !a.Cards[j].Board.DateLastActivity.IsZero() && a.Cards[i].Board.DateLastActivity.After(*(a.Cards[j].Board.DateLastActivity))))
}

func (a byPriority) Less(i, j int) bool {
	// todo: replace with bit mask
	if a.Assigned(i, j) {
		return true
	}
	if a.Assigned(j, i) {
		return false
	}
	if a.DueBefore(i, j) {
		return true
	}
	if a.DueBefore(j, i) {
		return false
	}
	if a.VotesMore(i, j) {
		return true
	}
	if a.VotesMore(j, i) {
		return false
	}
	if a.PosLess(i, j) {
		return true
	}
	if a.PosLess(j, i) {
		return false
	}

	if a.LastBorderActivityMoreRecent(i, j) {
		return true
	}
	if a.LastBorderActivityMoreRecent(j, i) {
		return false
	}

	if a.LastActivityOlder(i, j) {
		return true
	}

	return false
}

type byNewest []*t.Board

func (a byNewest) Len() int {
	return len(a)
}

func (a byNewest) Swap(i, j int) {
	a[i], a[j] = a[j], a[i]
}

func (a byNewest) Less(i, j int) bool {
	return boardTimeForSorting(a[i]) > boardTimeForSorting(a[j])
}

type byActuality []*t.Label

func (a byActuality) Len() int {
	return len(a)
}

func (a byActuality) Swap(i, j int) {
	a[i], a[j] = a[j], a[i]
}

func (a byActuality) Less(i, j int) bool {
	return a[i].Uses > a[j].Uses || (a[i].Uses == a[j].Uses) && a[i].Name != "" && a[j].Name == ""
}

func boardTimeForSorting(b *t.Board) int64 {
	if b.DateLastActivity == nil {
		if b.DateLastView == nil {
			return 0
		}
		return b.DateLastView.Unix()
	}
	return b.DateLastActivity.Unix()
}

func existsWebhookByBoard(c *integram.Context, boardID string) (webhookInfo, error) {
	res, err := api(c).Request("GET", "tokens/"+c.User.OAuthToken()+"/webhooks", nil, nil)
	if err != nil {
		return webhookInfo{}, err
	}

	webhooks := []webhookInfo{}

	err = json.Unmarshal(res, &webhooks)
	if err != nil {
		return webhookInfo{}, err
	}
	log.Printf("webhooks: %+v\n", webhooks)
	for _, webhook := range webhooks {
		if webhook.IDModel == boardID && webhook.CallbackURL == c.User.ServiceHookURL() {
			return webhook, nil
		}
	}

	return webhookInfo{}, errors.New("not found")
}

func resubscribeAllBoards(c *integram.Context) error {
	us := userSettings(c)
	any := false
	if us.Boards != nil {
		uToken := c.User.OAuthToken()
		for id, board := range us.Boards {
			if board.OAuthToken != uToken || board.TrelloWebhookID == "" {
				// todo: make a job
				qp := url.Values{"description": {"Integram"}, "callbackURL": {c.User.ServiceHookURL()}, "idModel": {id}}
				webhook := webhookInfo{}

				res, err := api(c).Request("POST", "tokens/"+uToken+"/webhooks", nil, qp)
				if err != nil {
					c.Log().WithError(err).Error("resubscribeAllBoards")
				} else {
					err = json.Unmarshal(res, &webhook)
					if err != nil {
						c.Log().WithError(err).Error("resubscribeAllBoards")
					} else {
						board.OAuthToken = uToken
						board.TrelloWebhookID = webhook.ID
						us.Boards[id] = board
						any = true
					}
				}
			}
		}
	}
	if !any {
		return nil
	}
	return c.User.SaveSettings(us)
}

func scheduleSubscribeIfBoardNotAlreadyExists(c *integram.Context, b *t.Board, chatID int64) error {
	us := userSettings(c)

	if b == nil {
		c.Log().Error("scheduleSubscribeIfBoardNotAlreadyExists nil board")
		return nil
	}

	if us.Boards != nil {
		if val, exists := us.Boards[b.Id]; exists {
			if val.OAuthToken == c.User.OAuthToken() {

				c.Chat.ID = chatID
				cs := chatSettings(c)
				if _, chatBoardExists := cs.Boards[b.Id]; chatBoardExists {
					sendBoardFiltersKeyboard(c, b.Id)
					return nil
				}
				return processWebhook(c, b, chatID, val.TrelloWebhookID)
			}
		}
	}

	_, err := c.Service().SheduleJob(subscribeBoard, 0, time.Now(), c, b, chatID)
	return err
}

func processWebhook(c *integram.Context, b *t.Board, chatID int64, webhookID string) error {
	boardSettings := UserBoardSetting{Name: b.Name, TrelloWebhookID: webhookID, OAuthToken: c.User.OAuthToken()}
	if chatID != 0 {
		c.User.AddChatToHook(chatID)
		cs := &ChatSettings{}
		initiatedInThePrivateChat := false
		if c.Chat.ID != chatID {
			initiatedInThePrivateChat = true
		}
		c.Chat.Settings(cs)
		if cs.Boards == nil {
			cs.Boards = make(map[string]ChatBoardSetting)
		}
		c.Chat.ID = chatID
		cs.Boards[b.Id] = ChatBoardSetting{Name: b.Name, Enabled: true, User: c.User.ID, Filter: defaultBoardFilter}
		err := c.Chat.SaveSettings(cs)
		if err != nil {
			return err
		}
		buttons := integram.Buttons{{b.Id, "🔧 Tune board " + b.Name}, {"anotherone", "➕ Add another one"}, {"done", "✅ Done"}}

		if c.Chat.IsGroup() {
			var msgWithButtons *integram.OutgoingMessage
			if initiatedInThePrivateChat {
				c.NewMessage().
					SetText(fmt.Sprintf("Board \"%s\" integrated", b.Name)).
					HideKeyboard().
					SetChat(c.User.ID).
					Send()

				msgWithButtons = c.NewMessage().
					SetText(fmt.Sprintf("%s was integrated board \"%s\" here. You can reply my messages to comment cards", c.User.Mention(), b.Name)).
					SetChat(chatID)
			} else {
				msgWithButtons = c.NewMessage().
					SetText(fmt.Sprintf("%s was integrated board \"%s\" here. You can reply my messages to comment cards", c.User.Mention(), b.Name)).
					SetChat(chatID)
			}

			msgWithButtons.
				SetKeyboard(buttons, true).
				SetOneTimeKeyboard(true).
				SetReplyAction(afterBoardIntegratedActionSelected).
				Send()

		} else {
			c.NewMessage().
				SetText(fmt.Sprintf("Board \"%s\" integrated here", b.Name)).
				SetKeyboard(buttons, true).
				SetOneTimeKeyboard(true).
				SetChat(c.User.ID).
				SetReplyAction(afterBoardIntegratedActionSelected).
				Send()
		}
	}

	s := userSettings(c)
	if s.Boards == nil {
		s.Boards = make(map[string]UserBoardSetting)
	}
	s.Boards[b.Id] = boardSettings

	c.User.SaveSettings(s)
	return nil
}

func afterBoardIntegratedActionSelected(c *integram.Context) error {
	key, _ := c.KeyboardAnswer()

	if key == "anotherone" {
		// we can use directly because of boards cached
		sendBoardsToIntegrate(c)
		return nil
	} else if key != "done" {
		sendBoardFiltersKeyboard(c, key)
	}

	return nil
}

func authWasRevokedMessage(c *integram.Context) {
	c.User.ResetOAuthToken()
	c.NewMessage().EnableAntiFlood().SetTextFmt("Looks like you have revoked the Integram access. In order to use me you need to authorize again: %s", oauthRedirectURL(c)).SetChat(c.User.ID).Send()
}

func subscribeBoard(c *integram.Context, b *t.Board, chatID int64) error {
	qp := url.Values{"description": {"Integram"}, "callbackURL": {c.User.ServiceHookURL()}, "idModel": {b.Id}}

	_, err := api(c).Request("POST", "tokens/"+c.User.OAuthToken()+"/webhooks", nil, qp)
	webhook := webhookInfo{}
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			webhook, err = existsWebhookByBoard(c, b.Id)
			if err != nil {
				c.Log().WithError(err).WithField("boardID", b.Id).Error("Received ErrorWebhookExists but can't refetch")
				return err
			}
		} else if strings.Contains(err.Error(), "401 Unauthorized") {
			authWasRevokedMessage(c)
			c.User.SetAfterAuthAction(subscribeBoard, b, chatID)
			return nil
		} else {
			return err
		}
	} else {

		webhook, err = existsWebhookByBoard(c, b.Id)
		if err != nil {
			return err
		}

		// instead of checking the provided webhook lets query Trello to ensure it has a webhook for sure
		// in some cases Trello provides webhook but doesn't actually store it
		/*err = json.Unmarshal(res, &webhook)
		if err != nil {
			return err
		}*/
	}

	return processWebhook(c, b, chatID, webhook.ID)
}

func labelsFilterByID(labels []*t.Label, id string) *t.Label {
	for _, label := range labels {
		if label.Id == id {
			return label
		}
	}
	return nil
}

func membersFilterByID(members []*t.Member, id string) *t.Member {
	for _, member := range members {
		if member.Id == id {
			return member
		}
	}
	return nil
}

func boardsFilterByID(boards []*t.Board, id string) *t.Board {
	for _, board := range boards {
		if board.Id == id {
			return board
		}
	}
	return nil
}

func listsFilterByID(lists []*t.List, id string) *t.List {
	for _, list := range lists {
		if list.Id == id {
			return list
		}
	}
	return nil
}

func targetChatSelected(c *integram.Context, boardID string) error {
	if boardID == "" {
		err := errors.New("BoardID is empty")
		return err
	}

	key, _ := c.KeyboardAnswer()
	var chatID int64
	boards, _ := boards(c, api(c))
	board := boardsFilterByID(boards, boardID)

	if key == "group" {
		// Defer adding chatID by using Telegram's ?startgroup
		chatID = 0
		c.NewMessage().
			SetText(fmt.Sprintf("Use this link to choose the group chat: https://telegram.me/%s?startgroup=%s", c.Bot().Username, boardID)).
			HideKeyboard().
			DisableWebPreview().
			Send()

	} else if key == "private" {
		chatID = c.User.ID
	} else if key != "" {
		var err error
		chatID, err = strconv.ParseInt(key, 10, 64)

		if err != nil {
			return err
		}

	}

	return scheduleSubscribeIfBoardNotAlreadyExists(c, board, chatID)
}

func renderBoardFilters(c *integram.Context, boardID string, keyboard *integram.Keyboard) error {
	cs := chatSettings(c)
	if bs, ok := cs.Boards[boardID]; ok {
		if bs.Enabled == false {
			(*keyboard) = (*keyboard)[0:1]
		}
		for rowIndex, row := range *keyboard {
			for colIndex, button := range row {
				if button.Data == "switch" {
					if bs.Enabled == true {
						(*keyboard)[rowIndex][colIndex].Text = "☑️ Notifications enabled"
					} else {
						(*keyboard)[rowIndex][colIndex].Text = "Turn on notifications"
					}
				} else {
					v := reflect.ValueOf(bs.Filter).FieldByName(button.Data)
					if bs.Enabled && v.IsValid() && v.Bool() {
						(*keyboard)[rowIndex][colIndex].Text = markSign + button.Text
					}
				}
			}
		}
		return nil
	}
	return errors.New("Can't find board settings on user")
}

func storeCard(c *integram.Context, card *t.Card) {
	c.SetServiceCache("card_"+card.Id, card, time.Hour*24*100)
	var cards []*t.Card
	c.User.Cache("cards", &cards)

	alreadyExists := false
	for _, cardR := range cards {
		if cardR.Id == card.Id {
			alreadyExists = true
			break
		}
	}

	if !alreadyExists {
		err := c.User.UpdateCache("cards", bson.M{"$addToSet": bson.M{"val": card}}, &cards)
		if err != nil {
			c.Log().WithError(err).Errorf("Cards cache update error")
		}
	}
}

func getBoardFilterKeyboard(c *integram.Context, boardID string) *integram.Keyboard {
	keyboard := integram.Keyboard{}

	keyboard.AddRows(
		integram.Buttons{{"switch", "🚫 Turn off all"}, {"finish", "🏁 Finish tunning"}},
		integram.Buttons{{"CardCreated", "Card Created"}, {"CardCommented", "Commented"}, {"CardMoved", "Moved"}},
		integram.Buttons{{"PersonAssigned", "Someone Assigned"}, {"Labeled", "Label attached"}, {"Voted", "Upvoted"}},
		integram.Buttons{{"Due", "Due date set"}, {"Checklisted", "Checklisted"}, {"Archived", "Archived"}},
	)

	renderBoardFilters(c, boardID, &keyboard)
	return &keyboard
}

func sendBoardFiltersKeyboard(c *integram.Context, boardID string) error {
	boards, _ := boards(c, api(c))
	board := boardsFilterByID(boards, boardID)

	if board == nil {
		return fmt.Errorf("board not found %v", boardID)
	}

	keyboard := getBoardFilterKeyboard(c, boardID)
	msg := c.NewMessage()

	if c.Message != nil {
		msg.SetReplyToMsgID(c.Message.MsgID)
	}
	return msg.
		SetText(fmt.Sprintf("%v tune notifications for \"%v\" board", c.User.Mention(), board.Name)).
		SetKeyboard(keyboard, true).
		SetSilent(true).
		SetReplyToMsgID(c.Message.MsgID).
		SetReplyAction(boardFilterButtonPressed, boardID).
		Send()
}

func cleanMarkSign(s string) string {
	if strings.HasPrefix(s, markSign) {
		return s[len(markSign):]
	}
	return s
}

func boardFilterButtonPressed(c *integram.Context, boardID string) error {
	answer, _ := c.KeyboardAnswer()
	if answer == "finish" {
		return c.NewMessage().
			SetText("Ok!").
			SetSilent(true).
			SetReplyToMsgID(c.Message.MsgID).
			HideKeyboard().
			Send()
	}

	cs := chatSettings(c)
	if bs, ok := cs.Boards[boardID]; ok {

		if answer == "switch" {
			bs.Enabled = !bs.Enabled
			cs.Boards[boardID] = bs
			c.Chat.SaveSettings(cs)

			keyboard := getBoardFilterKeyboard(c, boardID)
			onOrOff := "on"
			if !bs.Enabled {
				onOrOff = "off"
			}
			return c.NewMessage().
				SetText(c.User.Mention()+", all notifications turned "+onOrOff).
				SetKeyboard(keyboard, true).
				SetSilent(true).
				SetReplyToMsgID(c.Message.MsgID).
				SetReplyAction(boardFilterButtonPressed, boardID).
				Send()

		}
		v := reflect.ValueOf(&bs.Filter).Elem().FieldByName(answer)

		if v.IsValid() && v.CanSet() {
			v.SetBool(!v.Bool())

			cs.Boards[boardID] = bs
			c.Chat.SaveSettings(cs)
			/*var s string
			if v.Bool() {
				s = "enabled"
			} else {
				s = "disabled"
			}*/
			keyboard := getBoardFilterKeyboard(c, boardID)

			return c.NewMessage().
				SetText(
					decent.Shuffle(
						"Ok, %v",
						"Have done, %v",
						"I changed it, %v",
						"Done it for you, %v",
						"👌 %v",
						"👍 %v").
						S(c.User.Mention())).
				SetKeyboard(keyboard, true).
				SetReplyToMsgID(c.Message.MsgID).
				SetSilent(true).
				SetReplyAction(boardFilterButtonPressed, boardID).
				Send()
		}

	}
	return errors.New("Can't change board filter value")
}

func boardToTuneSelected(c *integram.Context) error {
	boardID, _ := c.KeyboardAnswer()
	if boardID == "" {
		return errors.New("Empty boardID")
	}

	return sendBoardFiltersKeyboard(c, boardID)
}

func boardToIntegrateSelected(c *integram.Context) error {
	boardID, boardName := c.KeyboardAnswer()
	log.Infof("boardToIntegrateSelected %s (%s)", boardName, boardID)

	if c.Chat.IsGroup() {
		boards, _ := boards(c, api(c))
		board := boardsFilterByID(boards, boardID)

		if board == nil {
			c.Log().Errorf("boardToIntegrateSelected, boardsFilterByID returned nil (id=%s, len(boards)=%d)", boardID, len(boards))
			return errors.New("board with id not found")
		}

		return scheduleSubscribeIfBoardNotAlreadyExists(c, board, c.Chat.ID)
	}
	but := integram.Buttons{}

	if tc := userSettings(c); tc.TargetChat != nil && tc.TargetChat.ID != 0 {
		but.Append(strconv.FormatInt(tc.TargetChat.ID, 10), tc.TargetChat.Title)
	}

	but.Append("group", "Choose the group")
	but.Append("private", "Private messages")

	c.NewMessage().
		SetText("Please choose where you would like to receive Trello notifications for board "+boardName).
		SetKeyboard(but.Markup(1), true).
		SetReplyToMsgID(c.Message.MsgID).
		SetReplyAction(targetChatSelected, boardID).
		Send()
	return nil
}

func boardForCardSelected(c *integram.Context) error {
	boardID, boardName := c.KeyboardAnswer()

	lists, err := listsByBoardID(c, api(c), boardID)
	if err != nil {
		return err
	}
	but := integram.Buttons{}
	for _, list := range lists {
		but.Append(list.Id, list.Name)
	}

	c.NewMessage().
		SetText("Please choose list for card in "+boardName).
		SetKeyboard(but.Markup(3), true).
		SetReplyToMsgID(c.Message.MsgID).
		SetOneTimeKeyboard(true).
		SetReplyAction(listForCardSelected, boardID, boardName).
		Send()
	return nil
}

func listForCardSelected(c *integram.Context, boardID string, boardName string) error {
	listID, listName := c.KeyboardAnswer()

	lists, err := listsByBoardID(c, api(c), boardID)
	if err != nil {
		return err
	}

	list := listsFilterByID(lists, listID)

	if list == nil {
		return errors.New("wrong listID " + listID + " listname " + listName)
	}
	return c.NewMessage().
		SetTextFmt("Enter the title. Card will be added to %s / %s ", m.Bold(boardName), m.Bold(listName)).
		EnableHTML().
		EnableForceReply().
		HideKeyboard().
		SetReplyAction(textForCardEntered, boardID, boardName, listID, listName).
		Send()
}

func colorEmoji(color string) string {
	switch color {
	case "yellow":
		return "🍋"
	case "red":
		return "🍎"
	case "blue":
		return "🔵"
	case "green":
		return "🍏"
	case "orange":
		return "🍊"
	case "purple":
		return "🍆"
	case "black":
		return "⚫️"
	case "pink":
		return "🎀"
	case "sky":
		return "💎"
	case "lime":
		return "🎾"
	default:
		return m.Italic(color)

	}
}

func cardText(c *integram.Context, card *t.Card) string {
	text := ""
	if card.Closed {
		text += "📦 <b>Card archived</b>\n"
	}
	by := ""
	if card.MemberCreator != nil {
		by = m.EncodeEntities(card.MemberCreator.FullName)
	}

	text += m.EncodeEntities(card.Name) + " " + m.URL("➔", c.WebPreview("by "+by, cardPath(card), "", card.URL(), ""))

	if card.Desc != "" {
		// todo: replace markdown in desc with html?
		card.Desc = cleanDesc(card.Desc)
		if card.Desc != "" {
			text += "\n" + card.Desc
		}
	}
	/* Space between card text and add info
	if len(card.Labels) > 0  || len(card.Members)>0 || len(card.Checklists)>0 || card.Due != nil && !card.Due.IsZero() {
		text += "\n"
	}*/
	if len(card.Labels) > 0 {
		text += "\n  "

		for i, label := range card.Labels {
			text += colorEmoji(label.Color) + " " + m.Bold(label.Name)
			if i < len(card.Labels)-1 && label.Name != "" {
				text += " "
			}
		}
	}

	if len(card.Members) > 0 {
		text += "\n  👤 "

		for i, member := range card.Members {
			text += mention(c, member)
			if i < len(card.Members)-1 {
				text += ", "
			}
		}
	}

	if card.Due != nil && !card.Due.IsZero() {
		text += "\n  📅 " + decent.Relative(card.Due.In(c.User.TzLocation()))
	}

	if len(card.Checklists) > 0 {
		for _, checklist := range card.Checklists {
			if len(checklist.CheckItems) > 0 {
				text += "\n  🚩 " + m.Bold(checklist.Name) + "\n"
				for _, checkItem := range checklist.CheckItems {
					if checkItem.State == "incomplete" {
						text += "       ⬜️ "
					} else {
						text += "       ✅ "
					}

					text += m.Italic(checkItem.Name) + "\n"
				}
			}
		}
	}

	text += "\n  📁 " + m.Bold(card.list.Name)

	return text
}

func cardInlineKeyboard(card *t.Card, more bool) integram.InlineKeyboard {
	but := integram.InlineButtons{}
	but.Append("assign", "Assign")

	var voteText string
	if len(card.IdMembersVoted) > 0 {
		voteText = fmt.Sprintf("👍 %d", len(card.IdMembersVoted))
	} else {
		voteText = "👍"
	}

	but.Append("move", "Move")

	but.Append("vote", voteText)
	if !more {
		but.Append("more", "…")
		return but.Markup(4, "actions")

	}
	but.Append("name", "Name")

	but.Append("desc", "Description")
	but.Append("due", "Due")
	// 65535 is the Trello default for first card in the list
	if card.Pos <= 65535 {
		but.AppendWithState(0, "position", "⬇ Bottom")
	} else {
		but.AppendWithState(1, "position", "⬆ Top")
	}
	but.Append("label", "Label")
	if !card.Closed {
		but.AppendWithState(1, "archive", "Archive")
	} else {
		but.AppendWithState(0, "archive", "Unarchive")
	}

	but.Append("back", "↑ Less")
	return but.Markup(3, "actions")
}

func inlineCardButtonPressed(c *integram.Context, cardID string) error {
	log.WithField("data", c.Callback.Data).WithField("state", c.Callback.State).WithField("cardID", cardID).Debug("inlineCardButtonPressed")
	api := api(c)

	card, err := getCard(c, api, cardID)
	if !c.User.OAuthValid() {
		if c.User.IsPrivateStarted() {
			c.AnswerCallbackQuery("Open the private chat with Trello", false)
			c.User.SetCache("auth_redirect", true, time.Hour*24)
			redirectURL := fmt.Sprintf("%s/tz?r=%s", integram.Config.BaseURL, url.QueryEscape(fmt.Sprintf("/oauth1/%s/%s", c.ServiceName, c.User.AuthTempToken())))
			c.NewMessage().EnableAntiFlood().SetTextFmt("You need to authorize me in order to use Trello bot: %s", redirectURL).SetChat(c.User.ID).Send()
		} else {
			kb := c.Callback.Message.InlineKeyboardMarkup
			kb.AddPMSwitchButton(c.Bot(), "👉  Tap me to auth", "auth")
			c.EditPressedInlineKeyboard(kb)

			c.AnswerCallbackQuery("You need to authorize me\nUse the \"Tap me to auth\" button", true)
		}
		return nil
	}
	if err != nil {
		return err
	}

	if c.Callback.Message.InlineKeyboardMarkup.State == "move" {
		err := moveCard(c, api, c.Callback.Data, card)
		if err != nil {
			return err
		}
		c.Callback.Data = "back"
	}

	if c.Callback.Message.InlineKeyboardMarkup.State == "assign" && c.Callback.Data != "back" {
		log.Info("assign state ", c.Callback.State)
		unassign := false

		if c.Callback.State == cardMemberStateAssigned {
			unassign = true
		}
		member, unassigned, err := assignMemberID(c, api, c.Callback.Data, unassign, card)
		if err != nil {
			return err
		}

		if member != nil {

			if unassigned {
				err = c.EditPressedInlineButton(cardMemberStateUnassigned, "   @"+member.Username)
			} else {
				// c.EditMessageText(c.Callback.Message, cardText(c, card))

				err = c.EditPressedInlineButton(cardMemberStateAssigned, "✅ @"+member.Username)
			}
			return err
		}
	}
	if c.Callback.Message.InlineKeyboardMarkup.State == "label" && c.Callback.Data != "back" {
		log.Info("label state ", c.Callback.State)
		removeLabel := false

		if c.Callback.State == cardLabelStateAttached {
			removeLabel = true
		}
		label, unattached, err := attachLabelID(c, api, c.Callback.Data, removeLabel, card)
		if err != nil {
			return err
		}

		if label != nil {

			if unattached {
				err = c.EditPressedInlineButton(cardLabelStateUnattached, "   "+colorEmoji(label.Color)+" "+label.Name)
			} else {
				err = c.EditPressedInlineButton(cardLabelStateAttached, "✅ "+colorEmoji(label.Color)+" "+label.Name)
			}
			return err
		}
	}

	if c.Callback.Message.InlineKeyboardMarkup.State == "due" {
		if c.Callback.Data == "due_clear" {
			_, err := cardSetDue(c, card, "")
			if err != nil {
				return err
			}
		} else if c.Callback.Data == "due_manual" {
			msg := c.NewMessage()

			if c.User.IsPrivateStarted() {
				msg.SetChat(c.User.ID)
			} else {
				msg.SetReplyToMsgID(c.Callback.Message.MsgID)
			}

			if c.Message != nil {
				msg.SetReplyToMsgID(c.Message.MsgID)
			}

			err = msg.SetText(c.User.Mention()+" write the due date in the format `dd.MM hh:mm`").
				EnableForceReply().
				EnableHTML().
				SetSelective(true).
				SetKeyboard(integram.Button{"cancel", "Cancel"}, true).
				SetReplyAction(сardDueDateEntered, card).
				Send()
			if err != nil {
				return err
			}
		} else if c.Callback.Data != "back" {
			_, err := cardSetDue(c, card, c.Callback.Data)
			if err != nil {
				return err
			}
		}

		c.Callback.Data = "back"

	}

	switch c.Callback.Data {
	case "back":
		kb := cardInlineKeyboard(card, false)

		return c.EditPressedMessageTextAndInlineKeyboard(cardText(c, card), kb)
	case "more":
		kb := cardInlineKeyboard(card, true)

		return c.EditPressedMessageTextAndInlineKeyboard(cardText(c, card), kb)
	case "archive":

		closed := true

		if c.Callback.State == 0 {
			closed = false
		}

		_, err = api.Request("PUT", "cards/"+card.Id+"/closed", nil, url.Values{"value": {fmt.Sprintf("%v", closed)}})
		if t.IsBadToken(err) {
			c.User.ResetOAuthToken()
		}
		if err != nil {
			return err
		}
		c.UpdateServiceCache("card_"+card.Id, bson.M{"$set": bson.M{"val.closed": closed}}, card)
		if card.Closed {
			c.AnswerCallbackQuery("You archived card \""+card.Name+"\"", false)

			return c.EditPressedInlineButton(0, "Unarchive")
		}
		c.AnswerCallbackQuery("You unarchived card \""+card.Name+"\"", false)

		return c.EditPressedInlineButton(1, "Archive")

	case "position":

		if c.Callback.State == 1 {
			err = card.SetPosition("top")
		} else {
			err = card.SetPosition("bottom")
		}

		if err != nil {
			return err
		}

		c.UpdateServiceCache("card_"+card.Id, bson.M{"$set": bson.M{"val.pos": card.Pos}}, card)
		log.Infof("new card pos %v", card.Pos)
		if card.Pos <= 65535 {
			c.AnswerCallbackQuery("You moved card \""+card.Name+"\" to the top of the list", false)

			return c.EditPressedInlineButton(0, "⬇ To the bottom")
		}
		c.AnswerCallbackQuery("You moved card \""+card.Name+"\" to the bottom of the list", false)

		return c.EditPressedInlineButton(1, "⬆ To the top")
	case "due":
		buts := integram.InlineButtons{}

		if card.Due != nil && !card.Due.IsZero() {
			buts.Append("due_clear", "Clear the due date")
		}

		userLocation := c.User.TzLocation()
		t := now.New(time.Now().In(userLocation))

		buts.Append(t.EndOfDay().Format(dueDateFormat), "🔥 Today")
		buts.Append(t.EndOfDay().AddDate(0, 0, 1).Format(dueDateFormat), "Tomorrow")

		buts.Append(t.EndOfSunday().Format(dueDateFormat), "Sunday")
		buts.Append(t.EndOfSunday().AddDate(0, 0, 7).Format(dueDateFormat), "Next Sunday")

		buts.Append(t.EndOfMonth().Format(dueDateFormat), "End of this month")
		buts.Append(now.New(t.AddDate(0, 1, -1*t.Day()+3)).EndOfMonth().Format(dueDateFormat), "End of the next month")
		buts.Append("due_manual", "Enter the date")
		buts.Append("back", "← Back")

		return c.EditPressedMessageTextAndInlineKeyboard(cardText(c, card), buts.Markup(1, "due"))

	case "move":

		lists, err := listsByBoardID(c, api, card.Board.Id)
		if err != nil {
			return err
		}
		buts := integram.InlineButtons{}

		for _, list := range lists {
			if list.Id != card.List.Id {
				buts.Append(list.Id, list.Name)
			}
		}
		buts.Append("back", "↑ Less")

		return c.EditInlineKeyboard(c.Callback.Message, "actions", buts.Markup(1, "move"))
	case "label":
		buts, err := getCardLabelsButtons(c, api, card)
		if err != nil {
			return err
		}

		buts.Append("back", "← Back")

		// c.Callback.Message.SetCallbackAction(inlineCardAssignButtonPressed, cardID)

		kb := buts.Markup(1, "label")
		kb.FixedWidth = true
		return c.EditInlineKeyboard(c.Callback.Message, "actions", kb)
	case "assign":
		buts, err := getCardAssignButtons(c, api, card)
		if err != nil {
			return err
		}

		buts.Append("back", "← Back")

		kb := buts.Markup(1, "assign")
		kb.FixedWidth = true
		return c.EditInlineKeyboard(c.Callback.Message, "actions", kb)
	case "vote":
		me, err := me(c, api)
		if err != nil {
			return err
		}

		if !card.IsMemberVoted(me.Id) {
			_, err = api.Request("POST", "cards/"+card.Id+"/membersVoted", nil, url.Values{"value": {me.Id}})
			if err != nil && err.Error() == "400 Bad Request: member has already voted on the card" {
				err = nil
			}
			if err == nil {
				c.AnswerCallbackQuery("👍 You upvoted the \""+card.Name+"\"", false)
			} else {
				if t.IsBadToken(err) {
					c.User.ResetOAuthToken()
				}
			}
			// c.UpdateServiceCache("card_" + card.Id, bson.M{"$addToSet": bson.M{"val.membersvoted": me}}, card)
		} else {
			_, err = api.Request("DELETE", "cards/"+card.Id+"/membersVoted/"+me.Id, nil, nil)
			if err != nil && err.Error() == "400 Bad Request: member has not voted on the card" {
				err = nil
			}
			if err == nil {
				c.AnswerCallbackQuery("👎 You unvoted the \""+card.Name+"\"", false)
			} else {
				if t.IsBadToken(err) {
					c.User.ResetOAuthToken()
				}
			}
		}

		if err != nil {
			if strings.Contains(err.Error(), "unauthorized card permission requested") {
				c.AnswerCallbackQuery("First, you need to enable Voting Power-Up for this board", false)
				err = nil
			}
			return err
		}

	case "desc":
		msg := c.NewMessage()
		if c.User.IsPrivateStarted() {
			msg.SetChat(c.User.ID)
		} else {
			msg.SetReplyToMsgID(c.Callback.Message.MsgID)
		}
		desc := card.Desc
		if desc == "" {
			desc = "Description is empty"
		}
		return msg.SetText(m.Pre(desc)+"\n"+c.User.Mention()+", write the new description for the card").
			EnableForceReply().
			EnableHTML().
			SetSelective(true).
			SetKeyboard(integram.Button{"cancel", "Cancel"}, true).
			SetReplyAction(сardDescEntered, card).
			Send()

	case "name":
		msg := c.NewMessage()
		if c.User.IsPrivateStarted() {
			msg.SetChat(c.User.ID)
		} else {
			msg.SetReplyToMsgID(c.Callback.Message.MsgID)
		}

		return msg.SetText(m.Pre(card.Name)+"\n"+c.User.Mention()+", write the new name for the card").
			EnableForceReply().
			EnableHTML().
			SetSelective(true).
			SetKeyboard(integram.Button{"cancel", "Cancel"}, true).
			SetReplyAction(сardNameEntered, card).
			Send()
	}

	return nil
}

/*func getCardActionButtons(c *integram.Context, api *t.Client, card *t.Card) (integram.InlineButtons, error) {
	but := integram.InlineButtons{}

	members, err := membersByBoardID(c, api, card.Board.Id)
	// TODO: optimisation needed for double requesting members
	if err != nil {
		return but, err
	}

	if len(members) <= 3 {
		but, _ = getCardAssignButtons(c, api, card)
	} else {
		but.Append("assign", "Assign someone")
	}
	if card.Pos == 1 {
		but.AppendWithState(0, "position", "⬇️ To the bottom")
	} else {
		but.AppendWithState(1, "position", "⬆️ To the top")
	}

	but.Append("due", "📅 Set due date")
	but.Append("done", "✅ Done")
	return but, nil
}*/

func textForCardEntered(c *integram.Context, boardID string, boardName string, listID string, listName string) error {
	api := api(c)
	_, err := api.CreateCard(c.Message.Text, listID, nil)

	if t.IsBadToken(err) {
		c.User.ResetOAuthToken()
	}

	if err != nil {
		return err
	}

	return nil
}

func getCard(c *integram.Context, api *t.Client, cardID string) (*t.Card, error) {
	card := t.Card{}
	exists := c.ServiceCache("card_"+cardID, &card)

	if exists {
		card.SetClient(api)
		return &card, nil
	}
	cardE, err := api.Card(cardID)

	if t.IsBadToken(err) {
		c.User.ResetOAuthToken()
	}

	if err != nil {
		return nil, err
	}

	err = c.SetServiceCache("card_"+cardID, cardE, time.Hour*24*100)
	return cardE, err
}

func getCardAssignButtons(c *integram.Context, api *t.Client, card *t.Card) (integram.InlineButtons, error) {
	members, err := membersByBoardID(c, api, card.Board.Id)
	if err != nil {
		return integram.InlineButtons{}, err
	}
	but := integram.InlineButtons{}

	for _, member := range members {
		if card.IsMemberAssigned(member.Id) {
			but.AppendWithState(cardMemberStateAssigned, member.Id, "✅ @"+member.Username)
		} else {
			but.AppendWithState(cardMemberStateUnassigned, member.Id, "   @"+member.Username)
		}
	}
	return but, err
}

/*
func getCardChecklistsButtons(c *integram.Context, api *t.Client, card *t.Card) (integram.InlineButtons, error) {
	but := integram.InlineButtons{}


	if len(card.Checklists) > 0 {
		if len(card.Checklists) > 1 {
			for _, checklist := range card.Checklists {
				but.Append(checklist.Id, checklist.Name)
			}
		}else{


		}

		for _, checklist := range card.Checklists {
			if len(checklist.CheckItems) > 0 {
				text += "\n  🚩 " + m.Bold(checklist.Name) + "\n"
				for _, checkItem := range checklist.CheckItems {
					if checkItem.State == "incomplete" {
						text += "       ⬜️ "
					} else {
						text += "       ✅ "

					}


					text += m.Italic(checkItem.Name) + "\n"
				}
			}

		}
	}
	if err != nil {
		return integram.InlineButtons{}, err
	}

	for _, label := range labels {
		if card.IsLabelAttached(label.Id) {
			but.AppendWithState(CARD_LABEL_STATE_ATTACHED, label.Id, "✅ "+ colorEmoji(label.Color)+" "+label.Name)
		} else {
			but.AppendWithState(CARD_LABEL_STATE_NOTATTACHED, label.Id, "   "+ colorEmoji(label.Color)+" "+label.Name)
		}
	}
	return but, err
}*/

func getCardLabelsButtons(c *integram.Context, api *t.Client, card *t.Card) (integram.InlineButtons, error) {
	labels, err := labelsByBoardID(c, api, card.Board.Id)
	if err != nil {
		return integram.InlineButtons{}, err
	}
	but := integram.InlineButtons{}

	for _, label := range labels {
		if card.IsLabelAttached(label.Id) {
			but.AppendWithState(cardLabelStateAttached, label.Id, "✅ "+colorEmoji(label.Color)+" "+label.Name)
		} else {
			but.AppendWithState(cardLabelStateUnattached, label.Id, "   "+colorEmoji(label.Color)+" "+label.Name)
		}
	}
	return but, err
}

func cardSetDue(c *integram.Context, card *t.Card, date string) (string, error) {
	api := api(c)
	var err error
	var dt time.Time
	n := time.Now()

	if date == "" {
		_, err = api.Request("PUT", "cards/"+card.Id+"/due", nil, url.Values{"value": {"null"}})
		if t.IsBadToken(err) {
			c.User.ResetOAuthToken()
		}

		if err != nil {
			return "", err
		}

		card.Due = &time.Time{}
		return "", err

	}

	dt, err = time.ParseInLocation(dueDateFullFormat, date[0:5]+"."+fmt.Sprintf("%d", int(n.Year()))+date[5:], c.User.TzLocation())

	if err != nil {
		return "", err
	}

	dt = dt.In(time.UTC)

	if int(dt.Month()) < int(n.Month()) {
		dt = dt.AddDate(1, 0, 0)
	}

	log.WithField("due", dt.Format(time.RFC3339Nano)).Info("set due date")

	_, err = api.Request("PUT", "cards/"+card.Id+"/due", nil, url.Values{"value": {dt.Format(time.RFC3339Nano)}})

	if err != nil {
		if t.IsBadToken(err) {
			c.User.ResetOAuthToken()
		}
		return "", err
	}

	err = c.UpdateServiceCache("card_"+card.Id, bson.M{"$set": bson.M{"val.due": &dt}}, card)
	return dt.In(c.User.TzLocation()).Format(time.RFC1123Z), err
}

func сardNameEntered(c *integram.Context, card *t.Card) error {
	action, _ := c.KeyboardAnswer()
	if action == "cancel" {
		return c.NewMessage().SetText("Ok").HideKeyboard().Send()
	}

	api := api(c)
	card.SetClient(api)
	err := card.SetName(c.Message.Text)
	if err == nil {
		c.NewMessage().SetText("Ok").HideKeyboard().Send()
	}

	return err
}

func сardDescEntered(c *integram.Context, card *t.Card) error {
	action, _ := c.KeyboardAnswer()
	if action == "cancel" {
		return c.NewMessage().SetText("Ok").HideKeyboard().Send()
	}

	api := api(c)
	card.SetClient(api)
	err := card.SetDesc(c.Message.Text)
	if err == nil {
		c.NewMessage().SetText("Ok").HideKeyboard().Send()
	}

	return err
}

func сardDueDateEntered(c *integram.Context, card *t.Card) error {
	action, _ := c.KeyboardAnswer()
	if action == "cancel" {
		return c.NewMessage().SetText("Ok").HideKeyboard().Send()
	}
	if action == "" {
		action = c.Message.Text
	}

	_, err := cardSetDue(c, card, action)
	if err == nil {
		c.NewMessage().SetText("Ok").HideKeyboard().Send()
	}

	return err
}

/*func afterCardCreatedActionSelected(c *integram.Context, card *t.Card) error {
	fmt.Printf("afterCardCreatedActionSelected card:%+v\n", card)
	api := api(c)
	action, _ := c.KeyboardAnswer()
	var err error
	switch action {
	case "movebottom":

		_, err = api.Request("PUT", "cards/"+card.Id+"/pos", nil, url.Values{"value": {"bottom"}})
		if err != nil {
			return err
		}

		card.Pos = 999
		but, _ := getCardActionButtons(c, api, card)

		c.NewMessage().
			SetText("Now the card is at the bottom").
			SetKeyboard(but.Markup(3), false).
			SetReplyAction(afterCardCreatedActionSelected, card).
			Send()
	case "movetop":
		_, err = api.Request("PUT", "cards/"+card.Id+"/pos", nil, url.Values{"value": {"top"}})
		if err != nil {
			return err
		}

		card.Pos = 1
		but, _ := getCardActionButtons(c, api, card)

		c.NewMessage().
			SetKeyboard(but.Markup(3), false).
			SetText("Now the card is on the top").
			SetReplyAction(afterCardCreatedActionSelected, card).
			Send()
	case "assign":
		buttons, err := getCardAssignButtons(c, api, card)
		if err != nil {
			return err
		}
		c.NewMessage().
			SetText("Select person to assign").
			SetKeyboard(buttons.Markup(3), true).
			SetReplyAction(afterCardCreatedActionSelected, card).
			Send()
	case "due":
		buttons := integram.Buttons{}

		if card.Due != nil && !card.Due.IsZero() {
			buttons.Append("due_clear", "Clear the due date")
		}
		userLocation := c.User.TzLocation()
		t := now.New(time.Now().In(userLocation))

		buttons.Append(t.EndOfDay().Format(dueDateFormat), "🔥 Today")
		buttons.Append(t.EndOfDay().AddDate(0, 0, 1).Format(dueDateFormat), "Tomorrow")

		buttons.Append(t.EndOfSunday().Format(dueDateFormat), "Sunday")
		buttons.Append(t.EndOfSunday().AddDate(0, 0, 7).Format(dueDateFormat), "Next Sunday")

		buttons.Append(t.EndOfMonth().Format(dueDateFormat), "End of this month")
		buttons.Append(now.New(t.AddDate(0, 1, -1*t.Day()+3)).EndOfMonth().Format(dueDateFormat), "End of the next month")

		c.NewMessage().
			SetText("Select the due date or write in the format dd.MM hh:mm").
			SetKeyboard(buttons.Markup(3), true).
			SetReplyAction(сardDueDateEntered, card).
			Send()

	case "done":
		c.NewMessage().
			HideKeyboard().
			SetText("Ok!").
			Send()
	default:
		member, unassigned, err := assignMemberID(c, api, action, false, card)
		if err != nil {
			return err
		}

		var msgText string
		if unassigned {
			msgText = member.FullName + " unassigned"
		} else {
			msgText = member.FullName + " assigned"
		}

		but, _ := getCardActionButtons(c, api, card)

		c.NewMessage().
			SetKeyboard(but.Markup(3), false).
			SetText(msgText).
			SetReplyAction(afterCardCreatedActionSelected, card).
			Send()
	}

	return err
}*/

func multipartBody(params url.Values, paramName, path string) (b *bytes.Buffer, contentType string, err error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile(paramName, filepath.Base(path))
	if err != nil {
		return nil, "", err
	}
	_, err = io.Copy(part, file)

	for key, val := range params {
		_ = writer.WriteField(key, val[0])
	}
	err = writer.Close()
	if err != nil {
		return nil, "", err
	}

	return body, writer.FormDataContentType(), nil
}

func moveCard(c *integram.Context, api *t.Client, listID string, card *t.Card) error {
	m := regexp.MustCompile("[0-9abcdef]{24}")

	if !m.MatchString(listID) {
		return nil
	}
	var lists []*t.List
	var err error
	lists, err = listsByBoardID(c, api, card.Board.Id)
	if err != nil {
		return err
	}

	list := listsFilterByID(lists, listID)
	if list == nil {
		return errors.New("listID not found in board")
	}
	_, err = api.Request("PUT", "cards/"+card.Id+"/idList", nil, url.Values{"value": {list.Id}})
	if err != nil {
		if t.IsBadToken(err) {
			c.User.ResetOAuthToken()
		}
		return err
	}
	return c.UpdateServiceCache("card_"+card.Id, bson.M{"$set": bson.M{"val.list": list}}, card)
}

func attachLabelID(c *integram.Context, api *t.Client, labelID string, unattach bool, card *t.Card) (label *t.Label, unattached bool, err error) {
	m := regexp.MustCompile("[0-9abcdef]{24}")
	unattached = false

	if m.MatchString(labelID) {
		var labels []*t.Label
		labels, err = labelsByBoardID(c, api, card.Board.Id)
		if err != nil {
			return
		}

		label = labelsFilterByID(labels, labelID)
		if label != nil {
			alreadyAttached := -1

			for i, m := range card.Labels {
				if m.Id == label.Id {
					alreadyAttached = i
					break
				}
			}
			//			var b []byte
			if unattach && alreadyAttached > -1 {

				_, err = api.Request("DELETE", "cards/"+card.Id+"/idLabels/"+label.Id, nil, nil)
				if t.IsBadToken(err) {
					c.User.ResetOAuthToken()
				}

				if err == nil {
					unattached = true
					card.Labels = append(card.Labels[:alreadyAttached], card.Labels[alreadyAttached+1:]...)
					err = c.UpdateServiceCache("card_"+card.Id, bson.M{"$pull": bson.M{"val.labels": label}}, card)
				}

			} else if !unattach {
				_, err = api.Request("POST", "cards/"+card.Id+"/idLabels", nil, url.Values{"value": {label.Id}})
				if t.IsBadToken(err) {
					c.User.ResetOAuthToken()
				}

				if err == nil {
					unattached = false
					card.Labels = append(card.Labels, label)
					err = c.UpdateServiceCache("card_"+card.Id, bson.M{"$addToSet": bson.M{"val.labels": label}}, card)

				}

			}

		} else {
			err = fmt.Errorf("can't find labelID inside board %s", card.Board.Id)
		}
		// looks like member ID
	} else {
		err = fmt.Errorf("bad labelID %s", labelID)
	}
	return
}

func assignMemberID(c *integram.Context, api *t.Client, memberID string, unassign bool, card *t.Card) (member *t.Member, unassigned bool, err error) {
	m := regexp.MustCompile("[0-9abcdef]{24}")
	unassigned = false

	if m.MatchString(memberID) {
		var members []*t.Member
		members, err = membersByBoardID(c, api, card.Board.Id)
		if err != nil {
			return
		}

		member = membersFilterByID(members, memberID)
		if member != nil {
			alreadyAssigned := -1
			for i, m := range card.Members {
				if m.Id == member.Id {
					alreadyAssigned = i
					break
				}
			}
			//			var b []byte
			if unassign && alreadyAssigned > -1 {

				_, err = api.Request("DELETE", "cards/"+card.Id+"/idMembers/"+member.Id, nil, nil)

				if t.IsBadToken(err) {
					c.User.ResetOAuthToken()
				}

				if err == nil {
					unassigned = true
					card.Members = append(card.Members[:alreadyAssigned], card.Members[alreadyAssigned+1:]...)
					err = c.UpdateServiceCache("card_"+card.Id, bson.M{"$pull": bson.M{"val.members": member}}, card)
				}

			} else if !unassign {
				_, err = api.Request("POST", "cards/"+card.Id+"/idMembers", nil, url.Values{"value": {member.Id}})

				if t.IsBadToken(err) {
					c.User.ResetOAuthToken()
				}

				if err == nil {
					unassigned = false
					card.Members = append(card.Members, member)
					err = c.UpdateServiceCache("card_"+card.Id, bson.M{"$addToSet": bson.M{"val.members": member}}, card)

				}

			}

		} else {
			err = fmt.Errorf("can't find memberID inside board %f", card.Board.Id)
		}
		// looks like member ID
	} else {
		err = fmt.Errorf("bad memberID %s", memberID)
	}
	return
}

func boardsButtons(c *integram.Context) (*integram.Buttons, error) {
	boards, err := boards(c, api(c))
	if err != nil {
		return nil, err
	}
	sort.Sort(byNewest(boards))

	buttons := integram.Buttons{}
	for _, board := range boards {
		buttons.Append(board.Id, board.Name)
	}
	return &buttons, nil
}

func sendBoardsForCard(c *integram.Context) error {
	buttons, err := boardsButtons(c)
	if err != nil {
		return err
	}
	p := ""
	if c.Chat.IsGroup() {
		p = "Let's continue here. "
	}
	return c.NewMessage().
		SetText(p+"Select the board to create a card\n"+m.Bold("Tip: you can create new cards in a few seconds! Just type this in any chat: ")+m.Pre("@"+c.Bot().Username+" New card title")).
		SetChat(c.User.ID).
		EnableHTML().
		SetKeyboard(buttons.Markup(2), true).
		SetReplyAction(boardForCardSelected).
		Send()
}

func sendBoardsToIntegrate(c *integram.Context) error {
	buttons, err := boardsButtons(c)
	if err != nil {
		return err
	}
	text := ""
	if c.Chat.IsGroup() {
		text = fmt.Sprintf("%s select the board to integrate here. To use the different Trello account – you can /reauthorize me", c.User.Mention())
	} else {
		text = fmt.Sprintf("%s select the board. After that you'll be able to choose the chat to integrate it. To use the different Trello account – you can /reauthorize me", c.User.Mention())
	}
	msg := c.NewMessage()
	if c.Message != nil {
		msg.SetReplyToMsgID(c.Message.MsgID)
	}
	return msg.
		SetText(text).
		SetSilent(true).
		SetKeyboard(buttons.Markup(2), true).
		SetReplyAction(boardToIntegrateSelected).
		Send()
}

func inlineCardCreate(c *integram.Context, listID string) error {
	api := api(c)
	card, err := api.CreateCard(c.ChosenInlineResult.Query, listID, nil)

	if t.IsBadToken(err) {
		c.User.ResetOAuthToken()
	}

	if err != nil {
		return err
	}

	c.ChosenInlineResult.Message.AddEventID("card_" + card.Id)
	c.ChosenInlineResult.Message.SetCallbackAction(inlineCardButtonPressed, card.Id)
	c.ChosenInlineResult.Message.SetReplyAction(cardReplied, card.Id)
	// err = c.DeleteMessage(c.ChosenInlineResult.Message)
	err = c.ChosenInlineResult.Message.Update(c.Db())
	if err != nil {
		c.Log().WithError(err).Errorf("DeleteMessage error")
		return err
	}

	lists, err := listsByBoardID(c, api, card.IdBoard)
	if err != nil {
		return err
	}

	boards, err := boards(c, api)
	if err != nil {
		return err
	}
	list := listsFilterByID(lists, card.IdList)
	board := boardsFilterByID(boards, card.IdBoard)
	member, _ := me(c, api)

	card.Board = board
	card.List = list
	card.MemberCreator = member

	storeCard(c, card)

	return c.EditMessageTextAndInlineKeyboard(c.ChosenInlineResult.Message, "", cardText(c, card), cardInlineKeyboard(card, false))
}

func inlineGetExistingCard(c *integram.Context, cardID string) error {
	api := api(c)
	card, err := getCard(c, api, cardID)
	if err != nil {
		return err
	}

	c.ChosenInlineResult.Message.AddEventID("card_" + card.Id)
	c.ChosenInlineResult.Message.SetCallbackAction(inlineCardButtonPressed, card.Id)
	c.ChosenInlineResult.Message.SetReplyAction(cardReplied, card.Id)

	err = c.ChosenInlineResult.Message.Update(c.Db())

	if err != nil {
		return err
	}
	return c.EditMessageTextAndInlineKeyboard(c.ChosenInlineResult.Message, "", cardText(c, card), cardInlineKeyboard(card, false))
}

func chosenInlineResultHandler(c *integram.Context) error {
	r := strings.Split(c.ChosenInlineResult.ResultID, "_")

	if len(r) != 2 {
		return errors.New("Bad Inline query ResultID: " + c.ChosenInlineResult.ResultID)
	}

	if r[0] == "l" {
		return inlineCardCreate(c, r[1])
	} else if r[0] == "c" {
		return inlineGetExistingCard(c, r[1])
	}
	return nil
}

func cacheAllCards(c *integram.Context, boards []*t.Board) error {
	var cards []*t.Card
	api := api(c)
	for bi := 0; bi < len(boards) && bi < 5; bi++ {
		board := boards[bi]

		var bcards []*t.Card
		b, err := api.Request("GET", "boards/"+board.Id+"/cards", nil, url.Values{"filter": {"open"}, "fields": {"name,idMembers,idMembersVoted,pos,due,idBoard,idList,dateLastActivity"}})

		if t.IsBadToken(err) {
			c.User.ResetOAuthToken()
		}

		if err != nil {
			return err
		}

		err = json.Unmarshal(b, &bcards)

		if err != nil {
			return err
		}
		cards = append(cards, bcards...)

	}
	return c.User.SetCache("cards", cards, time.Hour)
}

func strPtr(s string) *string {
	return &s
}

func inlineQueryHandler(c *integram.Context) error {
	if !c.User.OAuthValid() {
		return c.AnswerInlineQueryWithPM("You need to auth me to use Trello bot here", "inline")
	}
	var res []interface{}
	api := api(c)
	maxSearchResults := 5

	if c.InlineQuery.Query == "" {
		maxSearchResults = 20
	}

	boards, err := boards(c, api)
	if err != nil {
		return err
	}
	sort.Sort(byNewest(boards))

	var cards []*t.Card

	for bi := 0; bi < len(boards) && bi < 5; bi++ {
		if strings.EqualFold(boards[bi].Name, c.InlineQuery.Query) {
			maxSearchResults = 20
		}
	}
	c.User.Cache("cards", &cards)

	if cards == nil {

		b, err := api.Request("GET", "members/me/cards", nil, url.Values{"filter": {"open"}, "fields": {"name,idMembers,idMembersVoted,pos,due,idBoard,idList,dateLastActivity"}})

		if t.IsBadToken(err) {
			c.User.ResetOAuthToken()
		}

		if err != nil {
			return err
		}

		err = json.Unmarshal(b, &cards)

		if err != nil {
			return err
		}

		c.Service().DoJob(cacheAllCards, c, boards)
	}

	boardByID := boardsMaps(boards)

	for i, card := range cards {
		if v, ok := boardByID[card.IdBoard]; ok {
			cards[i].Board = v
		}
	}
	meInfo, err := me(c, api)
	if err != nil {
		return err
	}

	d := byPriority{Cards: cards, MeID: meInfo.Id}
	sort.Sort(d)
	start, _ := strconv.Atoi(c.InlineQuery.Offset)

	// cards=t.Cards
	ci := 0
	total := 0

	listsByBoardIDMap := make(map[string][]*t.List)
	for ci = start; ci < len(d.Cards) && total < maxSearchResults; ci++ {

		var board *t.Board

		card := d.Cards[ci]

		q := strings.TrimSpace(strings.ToLower(c.InlineQuery.Query))

		if _, ok := boardByID[card.IdBoard]; !ok {
			continue
		}
		board = boardByID[card.IdBoard]

		// if user specify query - we can filter cards
		if len(q) > 0 && !strings.Contains(strings.ToLower(card.Board.Name), q) && !strings.Contains(strings.ToLower(card.Name), q) && !strings.Contains(strings.ToLower(card.Desc), q) {
			continue
		}

		var list *t.List

		if _, ok := listsByBoardIDMap[card.IdBoard]; !ok {
			var err error
			listsByBoardIDMap[card.IdBoard], err = listsByBoardID(c, api, card.IdBoard)
			if err != nil {
				return nil
			}
		}

		list = listsFilterByID(listsByBoardIDMap[card.IdBoard], card.IdList)

		if list == nil {
			continue
		}

		// for empty query (most relevant cards) ignore last list in the board
		if q == "" && listsByBoardIDMap[card.IdBoard][len(listsByBoardIDMap[card.IdBoard])-1].Id == list.Id {
			continue
		}

		res = append(res,
			tg.InlineQueryResultArticle{
				ID:          "c_" + card.Id,
				Type:        "article",
				Title:       card.Name,
				Description: list.Name + " • " + board.Name,
				ThumbURL:    "https://st.integram.org/trello/" + board.Prefs.Background + ".png",
				InputMessageContent: tg.InputTextMessageContent{
					ParseMode:             "HTML",
					DisableWebPagePreview: false,
					Text:                  card.Name + "\n\n<b>" + list.Name + " • " + board.Name + "</b>",
				},
				ReplyMarkup: &tg.InlineKeyboardMarkup{
					InlineKeyboard: [][]tg.InlineKeyboardButton{
						{
							{Text: "Getting the card...", CallbackData: strPtr("wait")},
						},
					},
				},
			})
		total++
	}

	nextOffset := ""

	// if this is discovery query (empty or board name) we can stop here
	if maxSearchResults == 20 {
		if (ci + 1) < len(cards) {
			nextOffset = strconv.Itoa(ci)
		}
		return c.AnswerInlineQueryWithResults(res, 60, true, nextOffset)
	}

	for bi := 0; bi < len(boards) && bi < 10 && total < 20; bi++ {
		lists, err := listsByBoardID(c, api, boards[bi].Id)

		if err != nil {
			c.Log().WithError(err).WithField("board", boards[bi].Id).Error("Can't get lists for board")
		} else {
			for li := 0; li < len(lists)-1 && total < 20; li++ {
				// todo: this little bit messy...
				total++
				res = append(res,
					tg.InlineQueryResultArticle{
						ID:          "l_" + lists[li].Id,
						Type:        "article",
						Title:       lists[li].Name + " • " + boards[bi].Name,
						Description: c.InlineQuery.Query,
						ThumbURL:    "https://st.integram.org/trello/new_" + boards[bi].Prefs.Background + ".png",
						InputMessageContent: tg.InputTextMessageContent{
							ParseMode:             "HTML",
							DisableWebPagePreview: false,
							Text:                  c.InlineQuery.Query + "\n\n<b>" + lists[li].Name + " • " + boards[bi].Name + "</b>",
						},
						ReplyMarkup: &tg.InlineKeyboardMarkup{
							InlineKeyboard: [][]tg.InlineKeyboardButton{
								{
									{Text: "Creating card...", CallbackData: strPtr("wait")},
								},
							},
						},
					})
			}
		}
	}
	return c.AnswerInlineQueryWithResults(res, 10, true, "")
}

func newMessageHandler(c *integram.Context) error {
	u, _ := iurl.Parse("https://trello.com")
	c.ServiceBaseURL = *u

	command, param := c.Message.GetCommand()

	if param == "silent" {
		command = ""
	}
	if c.Message.IsEventBotAddedToGroup() {
		command = "start"
	}

	switch command {
	case "new":
		var err error
		if c.User.OAuthValid() {
			_, err = c.Service().DoJob(sendBoardsForCard, c)
		} else {
			kb := integram.InlineKeyboard{}
			kb.AddPMSwitchButton(c.Bot(), "👉  Tap me to auth", "auth")

			err = c.NewMessage().SetReplyToMsgID(c.Message.MsgID).SetText("You need to auth me to be able to create cards").SetInlineKeyboard(kb).Send()
		}
		return err
	case "search":
		var err error
		kb := integram.InlineButtons{integram.InlineButton{Text: "Tap to see how it's works", SwitchInlineQuery: "bug"}}

		err = c.NewMessage().SetReplyToMsgID(c.Message.MsgID).SetText("To search and share cards just type in any chat " + m.Bold("@"+c.Bot().Username+" fragment of card's name")).EnableHTML().SetInlineKeyboard(kb.Markup(3, "")).Send()

		return err
	case "start":

		/*if param[0:2]=="g_" {
			chatId, _ := strconv.ParseInt(param[2:], 10, 64)
			chatId=chatId*(-1)
			if chatId<0 {
				us,_:=c.Bot().API.GetChatMember(tgbotapi.ChatMemberConfig{ChatID:chatId,UserID:c.User.ID})
				if us.User.ID>0{
					cs,_:=c.Bot().API.GetChat(tgbotapi.ChatConfig{ChatID:chatId})
					c.User.SaveSetting("TargetChat", )
				}
			}
		}*/
		if param == "auth" {
			if c.User.OAuthValid() {
				return c.NewMessage().SetText("You are already authed at Trello. You can help another members of your group to do this and use the full power of the Trello inside the Telegram").Send()
			}
			c.User.SetCache("auth_redirect", true, time.Hour*24)
		}

		if len(param) > 10 {
			log.Debugf("Start param recived: %+v\n", param)
			boards, _ := boards(c, api(c))
			board := boardsFilterByID(boards, param)
			if board != nil {
				scheduleSubscribeIfBoardNotAlreadyExists(c, board, c.Chat.ID)
				return nil
			}
		}
		if c.User.OAuthValid() {
			_, err := c.Service().DoJob(sendBoardsToIntegrate, c)
			return err
		}

		if c.Chat.Type == "channel" {
			return c.NewMessage().
				SetText("Sorry, channels support for @trello_bot will coming soon").
				Send()
		}

		if c.Chat.IsGroup() {
			c.User.SaveSetting("TargetChat", c.Chat)

			return c.NewMessage().
				SetText("Hi folks! Let's get some juicy Trello in the Telegram. Tap the button to authorize me (you will switch to private messages)").
				SetInlineKeyboard(integram.InlineButton{Text: "Tap me!", URL: c.Bot().PMURL("connect")}).
				HideKeyboard().
				Send()
		}
		return c.NewMessage().
			HideKeyboard().
			SetTextFmt("Hi human! Let's get some juicy Trello in the Telegram. Open this link to authorize me: %s", oauthRedirectURL(c)).
			Send()

	case "reauthorize":
		c.User.ResetOAuthToken()
		c.User.SetCache("me", nil, time.Second)
		c.User.SetCache("boards", nil, time.Second)
		c.User.SetCache("cards", nil, time.Second)

		return c.NewMessage().
			SetTextFmt("Open this link to authorize me: %s", oauthRedirectURL(c)).
			SetChat(c.User.ID).
			Send()
	case "connect", "boards":
		if c.User.OAuthValid() {
			_, err := c.Service().DoJob(sendBoardsToIntegrate, c)
			return err
		}
		if c.Chat.IsGroup() {
			// Save Chat ID to know the integration target in private chat
			c.User.SaveSetting("TargetChat", c.Chat)
		}
		return c.NewMessage().
			SetTextFmt("Open this link to authorize me: %s", oauthRedirectURL(c)).
			SetChat(c.User.ID).
			Send()
	case "cancel", "clean", "reset":
		return c.NewMessage().SetText("Clean").HideKeyboard().Send()
	}
	return nil
}

func oauthRedirectURL(c *integram.Context) string {
	return fmt.Sprintf("%s/tz?r=%s", integram.Config.BaseURL, url.QueryEscape(fmt.Sprintf("/oauth1/%s/%s", c.ServiceName, c.User.AuthTempToken())))
}
