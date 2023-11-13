package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
	"github.com/mmcdole/gofeed"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

const (
	urlPattern string = `(?i)https://(twitter\.com|x\.com)`
	//put your mongo uri
	MONGO_URI string = "mongodb://localhost:XXXXX"
)

type Account struct {
	ID        string `bson:"_id"`
	GuildID   string
	TwitterID string
	ChannelID string
	CreatedAt time.Time
}

func init() {

}

func main() {
	err := godotenv.Load(".env")
	if err != nil {
		fmt.Println(err)
	}
	token := os.Getenv("BOT_TOKEN")

	mongo, err := mongo.Connect(context.Background(), options.Client().ApplyURI(MONGO_URI))
	if err != nil {
		panic(err)
	}
	defer func() {
		if err = mongo.Disconnect(context.Background()); err != nil {
			panic(err)
		}
	}()

	if err := mongo.Ping(context.Background(), readpref.Primary()); err != nil {
		panic(err)
	}

	coll := mongo.Database("Kurage").Collection("Users")

	dc, err := discordgo.New("Bot " + token)
	if err != nil {
		fmt.Println(err)
	}
	dc.AddHandler(onReady)
	dc.AddHandler(onEvents)

	err = dc.Open()
	if err != nil {
		fmt.Println(err)
	}

	commands := []*discordgo.ApplicationCommand{
		{
			Name:        "add",
			Description: "這個用來追加追蹤的帳號喔",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "string-option",
					Description: "String option",
					Required:    true,
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "string-option2",
					Description: "String option2",
					Required:    true,
				},
			},
		}, {
			Name:        "list",
			Description: "這個用來查詢已追加的帳號唷",
		}, {
			Name:        "remove",
			Description: "用了的話會刪掉追蹤的帳號唷",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "twitterid",
					Description: "String option",
					Required:    true,
				},
			},
		},
	}

	dc.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		const (
			accountsLimit int    = 15
			caution       string = "警告！"
		)
		embed := &discordgo.MessageEmbed{Color: 0x819aff}
		data := i.ApplicationCommandData()
		cmd := data.Name
		if i.Member.Permissions&discordgo.PermissionAdministrator == 0 {
			embed.Title = caution
			embed.Description = "噗噗！你不是管理者啦 叫管理者來用"
			responseEmbed(s, i, embed)
		} else {
			switch cmd {
			case "add":
				if len(getAccount(coll, i.GuildID)) > accountsLimit {
					embed.Title = caution
					embed.Description = fmt.Sprintf("超過%d個ID不可以喔！", accountsLimit)
					responseEmbed(s, i, embed)
				} else if !(regexp.MustCompile(`^[a-zA-Z0-9_]+$`).MatchString(data.Options[0].StringValue()) || regexp.MustCompile("^[0-9]+$").MatchString(data.Options[1].StringValue())) {
					embed.Title = caution
					embed.Description = "輸入的ID不對啦！\n推特ID只能用半形英文數字跟底線\nDC頻道ID只能是數字喔！"
					responseEmbed(s, i, embed)
				} else {
					embed.Description = fmt.Sprintf("嗶嗶嗶！追加成功囉！\n%s:%s", data.Options[0].StringValue(), data.Options[1].StringValue())
					responseEmbed(s, i, embed)
					doc := Account{primitive.NewObjectID().Hex(), i.GuildID, data.Options[0].StringValue(), data.Options[1].StringValue(), time.Now()}
					_, err = coll.InsertOne(context.Background(), doc)
					if err != nil {
						fmt.Println("Failed to insert document into MongoDB:", err)
					}
				}
			case "list":
				embed.Description = "下面是已經登錄的帳號唷"
				for _, v := range getAccount(coll, i.GuildID) {
					embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
						Name:   fmt.Sprintf("@%s", v.TwitterID),
						Value:  v.ChannelID,
						Inline: true})
				}
				responseEmbed(s, i, embed)
			case "remove":
				embed.Description = fmt.Sprintf("成功把@%s丟到焚化爐燒掉囉 永別了！", data.Options[0].StringValue())
				responseEmbed(s, i, embed)
				_, err := coll.DeleteMany(context.Background(), struct{ GuildID, TwitterID string }{i.GuildID, data.Options[0].StringValue()})
				if err != nil {
					fmt.Println(err)
				}
			}
		}
	})

	registeredCommands := make([]*discordgo.ApplicationCommand, len(commands))
	for i, v := range commands {
		cmd, err := dc.ApplicationCommandCreate(dc.State.User.ID, "", v)
		if err != nil {
			fmt.Printf("Cannot create '%s' command: %s", v.Name, err)
		}
		registeredCommands[i] = cmd
	}

	go Feed(dc, coll)

	defer dc.Close()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	log.Println("Press Ctrl+C to exit")
	<-stop

	//Global commands take time to be added to the server Deletion does the same.
	registeredCommands, err = dc.ApplicationCommands(dc.State.User.ID, "")
	if err != nil {
		fmt.Printf("Could not fetch registered commands: %s", err)
	}
	for _, v := range registeredCommands {
		err := dc.ApplicationCommandDelete(dc.State.User.ID, "", v.ID)
		if err != nil {
			fmt.Printf("Cannot delete '%s' command: %s", v.Name, err)
		}
	}
	fmt.Printf("%s is dead", dc.State.User.Username)
}

func Feed(discord *discordgo.Session, coll *mongo.Collection) {
	cur, err := coll.Find(context.Background(), bson.D{})
	if err != nil {
		fmt.Println(err)
	}
	var results []Account
	for cur.Next(context.Background()) {
		var accounts Account
		if err := cur.Decode(&accounts); err != nil {
			fmt.Println(err)
			continue
		}
		results = append(results, accounts)
	}
	var wg sync.WaitGroup
	for _, v := range results {
		wg.Add(1)
		go func(account Account) {
			defer wg.Done()
			fmt.Printf("Twitterid:%s, chID:%s", account.TwitterID, account.ChannelID)
			fmt.Println()
			previousTweetGUID := ""
			for {
				fp := gofeed.NewParser()
				feed, err := fp.ParseURL(fmt.Sprintf("https://nitter.poast.org/%s/rss", account.TwitterID))
				if err != nil {
					fmt.Println("Error parsing RSS feed:", err)
					break
				}
				fmt.Println(previousTweetGUID)
				latestTweet := feed.Items[0]

				//Nitter's filter does not work
				if strings.Contains(latestTweet.Title, fmt.Sprintf("RT by @%s", account.TwitterID)) {
					break
				}

				if previousTweetGUID != latestTweet.GUID {
					_, err = discord.ChannelMessageSend(account.ChannelID, convertFeedUrl(latestTweet.Link))
					if err != nil {
						fmt.Println("Error sending message to Discord:", err)
						break
					}
					previousTweetGUID = latestTweet.GUID
				}
				time.Sleep(30 * time.Second)
			}
		}(v)
	}
	wg.Wait()
}

func convertFeedUrl(url string) string {
	re := regexp.MustCompile(`https://nitter\.(.+?)/(.+?)/status/(\d+)#m`)
	if re.MatchString(url) {
		matches := re.FindStringSubmatch(url)
		if len(matches) == 4 {
			username := matches[2]
			statusID := matches[3]
			url = fmt.Sprintf("https://fxtwitter.com/%s/status/%s", username, statusID)
		}
	}
	return url
}

func responseEmbed(s *discordgo.Session, i *discordgo.InteractionCreate, embed *discordgo.MessageEmbed) {
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed},
		},
	})
}

func getAccount(coll *mongo.Collection, guild string) []Account {
	cur, err := coll.Find(context.Background(), struct{ GuildID string }{guild})
	if err != nil {
		fmt.Println(err)
	}
	var results []Account
	for cur.Next(context.Background()) {
		var accounts Account
		if err := cur.Decode(&accounts); err != nil {
			fmt.Println(err)
			continue
		}
		results = append(results, accounts)
	}
	return results
}

func onReady(s *discordgo.Session, event *discordgo.Ready) {
	s.UpdateWatchStatus(0, "克巴巴")
}

func onEvents(s *discordgo.Session, m interface{}) {
	if mCreate, isMessageCreate := m.(*discordgo.MessageCreate); isMessageCreate {
		handleMessage(s, mCreate.Message)
	} else if mUpdate, isMessageUpdate := m.(*discordgo.MessageUpdate); isMessageUpdate {
		handleMessage(s, mUpdate.Message)
	}
}

func handleMessage(s *discordgo.Session, m *discordgo.Message) {
	if m != nil && m.Author != nil {
		if m.Author.ID != s.State.User.ID && regexp.MustCompile(urlPattern).MatchString(m.Content) {
			convertedMsg := convertUrl(m.Content)
			s.ChannelMessageSend(m.ChannelID, convertedMsg+" 來自"+m.Author.Mention())
			s.ChannelMessageDelete(m.ChannelID, m.ID)
		}
	}
}

func convertUrl(msg string) string {
	const fixTwitterUrl string = "https://fxtwitter.com"
	return regexp.MustCompile(urlPattern).ReplaceAllString(msg, fixTwitterUrl)
}
