package main

import (
	"fmt"
	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
	"log"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"time"
)

var s *discordgo.Session

// set up logging
var (
	WarningLogger *log.Logger
	InfoLogger    *log.Logger
	ErrorLogger   *log.Logger
)

func init() {
	InfoLogger = log.New(os.Stdout, "INFO: ", log.Ldate|log.Ltime|log.Lshortfile)
	WarningLogger = log.New(os.Stdout, "WARNING: ", log.Ldate|log.Ltime|log.Lshortfile)
	ErrorLogger = log.New(os.Stdout, "ERROR: ", log.Ldate|log.Ltime|log.Lshortfile)
}

// Load Discord
func init() {
	if err := godotenv.Load(".env"); err != nil {
		WarningLogger.Println("No .env file found, please ensure environment variables are set")
	}
	var BotToken = os.Getenv("TOKEN") // Bot access token

	var err error
	s, err = discordgo.New("Bot " + BotToken)
	if err != nil {
		ErrorLogger.Fatalf("Error creating Discord session, is your token invalid?", err)
		return
	}
}

type LLMSession struct {
	// the channel to send messages to
	ChannelID string

	// the user who started the session
	UserID string

	// the time the session was last active
	LastActive time.Time

	// the context of the session
	Context *[]string

	// the control channel for session processing
	CtrlChannel chan bool

	// processing flag for session processing
	Processing *bool
}

var responseDiscordMessages []discordgo.Message

// Called when the LLM has a new chunk
func llmChunk(channelId string, text string, newMessage bool) error {
	go s.ChannelTyping(channelId)
	// send a new message
	if newMessage {
		responseDiscordMessage, err := s.ChannelMessageSend(channelId, text)
		if err != nil {
			return err
		}
		responseDiscordMessages = append(responseDiscordMessages, *responseDiscordMessage)
	} else {
		// edit the last message
		idx := len(responseDiscordMessages) - 1
		_, err := s.ChannelMessageEdit(responseDiscordMessages[idx].ChannelID, responseDiscordMessages[idx].ID, text)
		if err != nil {
			return err
		}
	}
	return nil
}

// Called when the LLM is done
func llmDone(channelId string, full string) error {
	// edit last message with the full response
	idx := len(responseDiscordMessages) - 1
	_, err := s.ChannelMessageEdit(responseDiscordMessages[idx].ChannelID, responseDiscordMessages[idx].ID, full)
	if err != nil {
		return err
	}

	// send a message to the user to let them know the bot is done typing
	_, err = s.ChannelMessageSend(channelId, "*Done typing! Waiting for a response...*")

	return nil
}

var (
	openSessions = make(map[string]LLMSession)

	commands = []*discordgo.ApplicationCommand{
		{
			Name: "session",
			// All commands and options must have a description
			// Commands/options without description will fail the registration
			// of the command.
			Description: "Start a session with the bot, this will create a thread and allow you to chat with the bot.",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Name:        "content",
					Description: "Content of the message",
					Type:        discordgo.ApplicationCommandOptionString,
					Required:    true,
				},
			},
		},
		{
			Name: "q",
			// All commands and options must have a description
			// Commands/options without description will fail the registration
			// of the command.
			Description: "Prompt the bot in the current channel",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Name:        "content",
					Description: "Content of the message",
					Type:        discordgo.ApplicationCommandOptionString,
					Required:    true,
				},
			},
		},
		{
			Name: "end",
			// All commands and options must have a description
			// Commands/options without description will fail the registration
			// of the command.
			Description: "End the current session if one is active",
		},
	}

	commandHandlers = map[string]func(s *discordgo.Session, i *discordgo.InteractionCreate){
		"end": func(s *discordgo.Session, i *discordgo.InteractionCreate) {
			// show a loading reponse
			err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
			})
			if err != nil {
				replyError(i, err)
				return
			}

			// check if there is an active session
			if session, ok := openSessions[i.ChannelID]; ok {
				// end the session
				if *session.Processing {
					session.CtrlChannel <- true
				}
				delete(openSessions, i.ChannelID)

				// send confirmation message to the user
				var embeds []*discordgo.MessageEmbed
				embeds = append(embeds, &discordgo.MessageEmbed{
					Title:     "Session Ended: Bye!",
					Timestamp: time.Now().Format(time.RFC3339),
					Color:     15548997,
				})

				// send the embed message for the thread
				if _, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
					Embeds: embeds,
				}); err != nil {
					replyError(i, err)
					return
				}

			} else {
				// send a message to the user to let them know the bot is done typing
				_, err = s.ChannelMessageSend(i.ChannelID, "No active session!")
				if err != nil {
					replyError(i, err)
					return
				}
			}
		},
		"q": func(s *discordgo.Session, i *discordgo.InteractionCreate) {
			// show a loading reponse
			err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
			})
			if err != nil {
				replyError(i, err)
				return
			}

			if err := startSession(i.ChannelID, i.ApplicationCommandData().Options[0].StringValue(), i.Member.User.ID); err != nil {
				replyError(i, err)
				return
			}

			respondToInteraction(i)
		},
		"session": func(s *discordgo.Session, i *discordgo.InteractionCreate) {

			InfoLogger.Println(i.Member.Nick + " sent a message: " + i.ApplicationCommandData().Options[0].StringValue())

			// show a loading reponse
			err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
			})
			if err != nil {
				replyError(i, err)
				return
			}

			// create a thread
			thread, err := s.ThreadStart(i.ChannelID, "Tent Bot Session", discordgo.ChannelTypeGuildPublicThread, 60)

			if err := startSession(thread.ID, i.ApplicationCommandData().Options[0].StringValue(), i.Member.User.ID); err != nil {
				replyError(i, err)
				return
			}

			respondToInteraction(i)
		},
	}
)

func respondToInteraction(i *discordgo.InteractionCreate) {
	// build the embed message
	var embeds []*discordgo.MessageEmbed
	embeds = append(embeds, &discordgo.MessageEmbed{
		Title:       "Tent Bot Session",
		Description: "Prompt: " + i.ApplicationCommandData().Options[0].StringValue(),
		Timestamp:   time.Now().Format(time.RFC3339),
		Color:       15844367,
		Footer: &discordgo.MessageEmbedFooter{
			Text: "Please wait for Tent Bot to finish typing, then reply to this message with your response.",
		},
	})

	if _, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Embeds: embeds,
	}); err != nil {
		replyError(i, err)
		return
	}
}

func startSession(channelId string, message string, userId string) error {

	// create a channel that will be used to shut down the LLM stream
	LLMCtrlChannel := make(chan bool)

	// store the session data
	openSessions[channelId] = LLMSession{
		ChannelID:   channelId,
		UserID:      userId,
		LastActive:  time.Now(),
		CtrlChannel: LLMCtrlChannel,
		Context:     new([]string),
		Processing:  new(bool),
	}

	// add the user's input as the first message in the context
	*openSessions[channelId].Context = append(*openSessions[channelId].Context, message)

	// initialize the LLM stream and use the first context message as the prompt
	scanner, err := GetLLMStream(channelId, *openSessions[channelId].Context)
	if err != nil {
		return err
	}

	// process the stream asynchronously
	go func() {
		// set processing to true
		*openSessions[channelId].Processing = true
		fullMessage, err := ProcessLLMStream(channelId, scanner, 5, llmChunk, llmDone, LLMCtrlChannel)
		if err != nil {
			return
		}
		// if the session still exists, set the processing status
		if _, ok := openSessions[channelId]; ok {
			// set processing to false
			*openSessions[channelId].Processing = false

			// append the full message to the context of the current session
			contextPtr := openSessions[channelId].Context
			*contextPtr = append(*contextPtr, fullMessage)
		}
	}()

	return nil
}

func replyError(i *discordgo.InteractionCreate, err error) {
	_, errErr := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content: "Something went wrong: " + err.Error(),
	})
	if errErr != nil {
		ErrorLogger.Println("Failed to send followup message: " + errErr.Error())
		ErrorLogger.Println("Original error: " + err.Error())
	}
	return
}

// Register slash commands
func init() {
	s.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if h, ok := commandHandlers[i.ApplicationCommandData().Name]; ok {
			h(s, i)
		}
	})
}

const (
	//MinimumCharactersOnID ...
	MinimumCharactersOnID int = 16
)

var (
	//RegexUserPatternID ...
	RegexUserPatternID *regexp.Regexp = regexp.MustCompile(fmt.Sprintf(`^(<@(\d{%d,})>)`, MinimumCharactersOnID))
)

func main() {
	s.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		InfoLogger.Printf("Logged in as: %v#%v", s.State.User.Username, s.State.User.Discriminator)
	})
	err := s.Open()
	if err != nil {
		ErrorLogger.Fatalf("Cannot open the session: %v", err)
	}

	// message send handler
	s.AddHandler(func(s *discordgo.Session, r *discordgo.MessageCreate) {
		// Ignore all messages created by the bot itself
		if r.Author.ID == s.State.User.ID {
			return
		}

		// start session in current channel when user mentions bot
		// make sure we don't already have a session open in this channel
		if _, ok := openSessions[r.ChannelID]; !ok {
			InfoLogger.Printf("User %v sent a message: %v", r.Author.ID, r.Content)
			if RegexUserPatternID.MatchString(r.Content) {
				// get first match
				mention := RegexUserPatternID.FindStringSubmatch(r.Content)[0]
				// get id from mention
				mentionId := mention[2 : len(mention)-1]
				// check to make sure the mention is the bot
				if mentionId != s.State.User.ID {
					return
				}

				// start session
				messageWithoutMention := strings.Replace(r.Content, mention, "", 1)
				InfoLogger.Printf("User %v mentioned bot, starting session", r.Author.ID)
				err := startSession(r.ChannelID, messageWithoutMention, r.Author.ID)
				if err != nil {
					return
				}
			}

			return
		}

		// check if the channel matches any open sessions
		if session, ok := openSessions[r.ChannelID]; ok {
			// the user just sent a message to the bot in an open session
			// so get a response from the LLM and send it to the user
			InfoLogger.Printf("User %v sent a message in an open session, session processing: %v", r.Author.ID, *session.Processing)

			// check if the session is processing
			if *session.Processing {
				// if the session is processing, interrupt the stream and initiate a new one
				openSessions[r.ChannelID].CtrlChannel <- true
			}
			// get message content
			content := r.Content

			// add it to the session context
			contextPtr := openSessions[r.ChannelID].Context
			*contextPtr = append(*contextPtr, content)

			// create new stream
			scanner, err := GetLLMStream(r.ChannelID, *contextPtr)
			if err != nil {
				// send error message
				_, err := s.ChannelMessageSend(r.ChannelID, "Something went wrong: "+err.Error())
				if err != nil {
					ErrorLogger.Printf("Cannot send error message: %v", err)
				}
				return
			}

			// process the stream asynchronously
			go func() {
				// create a channel that will be used to shut down the LLM stream
				LLMCtrlChannel := make(chan bool)
				// update the session's control channel
				session.CtrlChannel = LLMCtrlChannel
				// update the time
				session.LastActive = time.Now()
				// set processing to true
				*openSessions[r.ChannelID].Processing = true
				fullMessage, err := ProcessLLMStream(r.ChannelID, scanner, 5, llmChunk, llmDone, session.CtrlChannel)
				if err != nil {
					// send error message
					_, err := s.ChannelMessageSend(r.ChannelID, "Something went wrong: "+err.Error())
					if err != nil {
						ErrorLogger.Printf("Cannot send error message: %v", err)
					}
					return
				}
				// set processing to false
				*openSessions[r.ChannelID].Processing = false

				// append the full message to the context of the current session
				contextPtr := openSessions[r.ChannelID].Context
				*contextPtr = append(*contextPtr, fullMessage)
			}()
		}
	})

	InfoLogger.Println("Adding commands...")
	registeredCommands := make([]*discordgo.ApplicationCommand, len(commands))
	for i, v := range commands {
		cmd, err := s.ApplicationCommandCreate(s.State.User.ID, "", v)
		if err != nil {
			log.Panicf("Cannot create '%v' command: %v", v.Name, err)
		}
		registeredCommands[i] = cmd
	}

	defer s.Close()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	InfoLogger.Println("Press Ctrl+C to exit")
	<-stop

	InfoLogger.Println("Removing commands...")

	for _, v := range registeredCommands {
		err := s.ApplicationCommandDelete(s.State.User.ID, "", v.ID)
		if err != nil {
			log.Panicf("Cannot delete '%v' command: %v", v.Name, err)
		}
	}

	InfoLogger.Println("Gracefully shutting down.")
}
