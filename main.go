package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"io/ioutil"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func FirstNonEmptyString(strings ...string) string {
	for _, s := range strings {
		if s != "" {
			return s
		}
	}
	return ""
}

type Frederica struct {
	slackClient    *slack.Client
	socketClient   *socketmode.Client
	gptTemperature float32
	gptMaxTokens   int
	botID          string
	botUserID      string
	preludes       []string
	postludes      []string
}

func convertConversation(messages []slack.Message, botID string) []string {
	var conversation []string
	for _, msg := range messages {
		if msg.User == "" || msg.Text == "" {
			continue
		}
		if msg.BotID == botID {
			conversation = append(conversation, "assistant: "+msg.Text)
		} else {
			conversation = append(conversation, "user: "+msg.Text)
		}
	}
	return conversation
}

func (fred *Frederica) truncateMessages(messages []string, maxTokens int) ([]string, error) {
	// keep latest messages to fit maxTokens
	var totalTokens int
	for i := len(messages) - 1; i >= 0; i-- {
		content := messages[i]
		totalTokens += len(content)
		if totalTokens > maxTokens {
			return messages[i+1:], nil
		}
	}
	return messages, nil
}

func (fred *Frederica) getLatestMessages(channelID, ts string, maxTokens int) ([]string, error) {
	log.Println("getting replies", channelID, ts)

	response, err := fred.slackClient.GetConversationHistory(&slack.GetConversationHistoryParameters{
		ChannelID: channelID,
		//Latest:    ts,
	})
	//replies, _, _, err := fred.slackClient.GetConversationReplies(&slack.GetConversationRepliesParameters{
	//	ChannelID: channelID,
	//	Timestamp: ts,
	//})
	if err != nil {
		return nil, fmt.Errorf("failed getting conversation history: %v", err)
	}
	replies := response.Messages
	if len(replies) == 0 {
		return nil, fmt.Errorf("failed getting conversation history: no messages")
	}
	for i, j := 0, len(replies)-1; i < j; i, j = i+1, j-1 {
		replies[i], replies[j] = replies[j], replies[i]
	}
	log.Println("got replies", len(replies))
	for _, msg := range replies {
		log.Printf("%s: %s %v %v", msg.User, msg.Text, msg.ThreadTimestamp, msg.Timestamp)
	}
	converted := convertConversation(replies, fred.botID)
	return fred.truncateMessages(converted, maxTokens)
}

func (fred *Frederica) getMessage(channelID, ts string) (*slack.Message, error) {
	replies, _, _, err := fred.slackClient.GetConversationReplies(&slack.GetConversationRepliesParameters{
		ChannelID: channelID,
		Timestamp: ts,
		Limit:     1,
	})
	if err != nil {
		return nil, fmt.Errorf("failed getting conversation history: %v", err)
	}
	if len(replies) == 0 {
		return nil, fmt.Errorf("failed getting conversation history: no messages")
	}
	return &replies[0], nil
}

func logMessages(messages []string) {
	log.Println("-----MESSAGES_BEGIN-----")
	for _, msg := range messages {
		log.Printf(msg)
	}
	log.Println("-----MESSAGES_END-----")
}

func (fred *Frederica) postOnChannel(channelID, message string) error {
	_, _, err := fred.slackClient.PostMessage(channelID, slack.MsgOptionText(message, false))
	if err != nil {
		return fmt.Errorf("failed posting message: %v", err)
	}
	return nil
}

// エラーを追跡するための ID を生成する。
// この ID はエラーが発生したときにユーザーに伝える。
func generateTraceID() string {
	// ランダムな6文字の文字列を生成
	n := 6
	letters := []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")
	b := make([]rune, n)
	for i := range b {
		r, err := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
		if err != nil {
			return string(b)
		}
		b[i] = letters[r.Int64()]
	}
	return string(b)
}

// postErrorMessage posts an error message on the thread
func (fred *Frederica) postErrorMessage(channelID, ts string, traceID string) {
	message := fmt.Sprintf("エラーが発生しました。また後で試してね。 %s", traceID)
	err := fred.postOnChannel(channelID, message)
	if err != nil {
		log.Printf("failed to access OpenAI API: %v\n", err)
	}
}

func (fred *Frederica) handleMention(ev *slackevents.AppMentionEvent) {
	if ev.BotID == fred.botID || ev.User == fred.botUserID {
		return
	}
	ts := FirstNonEmptyString(ev.ThreadTimeStamp, ev.TimeStamp)
	truncated, err := fred.getLatestMessages(ev.Channel, ts, 3000)
	if err != nil {
		log.Printf("ERROR: failed getting latest messages: %v\n", err)
		return
	}
	// prepend prelude to truncated
	truncated = append(append(fred.preludes, truncated...), fred.postludes...)
	logMessages(truncated)
	completion, err := fred.createChatCompletion(context.Background(), truncated)
	if err != nil {
		traceID := generateTraceID()
		fred.postErrorMessage(ev.Channel, ts, traceID)
		log.Printf("ERROR: failed creating chat completion %s: %v\n", traceID, err)
		return
	}
	err = fred.postOnChannel(ev.Channel, completion)
	if err != nil {
		log.Printf("ERROR: failed posting message: %v\n", err)
		return
	}
}

func (fred *Frederica) handleEventTypeEventsAPI(evt *socketmode.Event) error {
	eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
	if !ok {
		log.Printf("Ignored %+v\n", evt)
		return nil
	}
	log.Printf("Event received: %+v\n", eventsAPIEvent)
	fred.socketClient.Ack(*evt.Request)
	switch eventsAPIEvent.Type {
	case slackevents.CallbackEvent:
		innerEvent := eventsAPIEvent.InnerEvent
		switch ev := innerEvent.Data.(type) {
		case *slackevents.AppMentionEvent:
			go fred.handleMention(ev)
		case *slackevents.MemberJoinedChannelEvent:
			fmt.Printf("user %q joined to channel %q", ev.User, ev.Channel)
		}
	default:
		fred.socketClient.Debugf("unsupported Events API event received")
	}
	return nil
}

func (fred *Frederica) eventLoop() {
	for evt := range fred.socketClient.Events {
		switch evt.Type {
		case socketmode.EventTypeConnecting:
			log.Println("Connecting to Slack with Socket Mode...")
		case socketmode.EventTypeConnectionError:
			log.Println("Connection failed. Retrying later...")
		case socketmode.EventTypeConnected:
			log.Println("Connected to Slack with Socket Mode.")
		case socketmode.EventTypeEventsAPI:
			err := fred.handleEventTypeEventsAPI(&evt)
			if err != nil {
				log.Printf("failed handling event type events api: %v\n", err)
				continue
			}
		}
	}
}

func main() {
	// read from environmental variable

	botToken := os.Getenv("BOT_TOKEN")
	if botToken == "" {
		panic("BOT_TOKEN is not set")
	}

	slackAppToken := os.Getenv("SLACK_APP_TOKEN")
	if slackAppToken == "" {
		panic("SLACK_APP_TOKEN is not set")
	}

	systemMessage, found := os.LookupEnv("SYSTEM_MESSAGE")
	if !found {
		systemMessage = "assistant の名前はソフィアです"
	}

	var preludes []string
	preludes = append(preludes, "system: "+systemMessage)

	systemMessagePost, foundPost := os.LookupEnv("SYSTEM_MESSAGE_POST")
	if !foundPost {
		systemMessagePost = "" // 「以上の会話について、assistant は語尾に「ですわ」を付けて返答してください。」のような設定
	}

	var postludes []string
	postludes = append(postludes, "system: "+systemMessagePost)

	slackClient := slack.New(
		botToken,
		slack.OptionDebug(false),
		slack.OptionLog(log.New(os.Stdout, "api: ", log.Lshortfile|log.LstdFlags)),
		slack.OptionAppLevelToken(slackAppToken),
	)

	socketClient := socketmode.New(
		slackClient,
		socketmode.OptionDebug(false),
		socketmode.OptionLog(log.New(os.Stdout, "socketmode: ", log.Lshortfile|log.LstdFlags)),
	)

	authTestResponse, err := slackClient.AuthTest()
	if err != nil {
		panic(err)
	}
	fred := &Frederica{
		slackClient:  slackClient,
		socketClient: socketClient,
		botID:        authTestResponse.BotID,
		botUserID:    authTestResponse.UserID,
		preludes:     preludes,
		postludes:    postludes,
	}

	go fred.eventLoop()

	err = socketClient.Run()
	if err != nil {
		panic(fmt.Errorf("failed running slack client: %w", err))
	}
}

func (fred *Frederica) createChatCompletion(ctx context.Context, messages []string) (string, error) {
	u := &url.URL{}
	u.Scheme = "http"
	u.Host = "127.0.0.1:3000"
	// url文字列
	uStr := u.String()
	// ポストデータ
	values := url.Values{}
	values.Add("message", strings.Join(messages, "\n"))

	// タイムアウトを30秒に指定してClient構造体を生成
	cli := &http.Client{Timeout: time.Duration(30) * time.Second}

	// POSTリクエスト発行
	rsp, err := cli.Post(uStr, "application/x-www-form-urlencoded", strings.NewReader(values.Encode()))
	if err != nil {
		fmt.Println(err)
		return "", nil
	}
	// 関数を抜ける際に必ずresponseをcloseするようにdeferでcloseを呼ぶ
	defer rsp.Body.Close()

	// レスポンスを取得し出力
	body, _ := ioutil.ReadAll(rsp.Body)
	return string(body), nil
}
