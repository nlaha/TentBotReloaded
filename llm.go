package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"github.com/Jeffail/gabs/v2"
	"github.com/bwmarrin/discordgo"
	"net/http"
	"strings"
	"time"
)

func GetLLMStream(channelId string, sessionContext []string) (*bufio.Scanner, error) {
	go s.ChannelTyping(channelId)
	// log
	// starting with session context
	InfoLogger.Println("Starting LLM stream with session context:")
	for idx, message := range sessionContext {
		if idx%2 == 0 {
			InfoLogger.Println("User: " + message)
		} else {
			InfoLogger.Println("Bot: " + message)
		}
	}

	// call llm api
	// JSON body
	reqBody := gabs.New()
	reqBody.Set(true, "stream")
	reqBody.Set(500, "max_tokens")
	reqBody.Array("messages")

	// add system message
	messageObj := gabs.New()
	messageObj.Set("system", "role")
	messageObj.Set("You are a helpful assistant running in the form of a discord bot named Tent Bot. You can send the following token to end the session with the user [[END]]", "content")
	reqBody.ArrayAppend(messageObj, "messages")

	for idx, message := range sessionContext {
		// every even index message is an "assistant" message
		if idx%2 == 0 {
			messageObj := gabs.New()
			messageObj.Set(message, "content")
			messageObj.Set("user", "role")
			reqBody.ArrayAppend(messageObj, "messages")
		} else {
			messageObj := gabs.New()
			messageObj.Set(message, "content")
			messageObj.Set("assistant", "role")
			reqBody.ArrayAppend(messageObj, "messages")
		}
	}

	req, err := http.NewRequest("POST", "https://llm.nlaha.com/v1/chat/completions", bytes.NewBuffer(reqBody.Bytes()))
	if err != nil {
		return nil, err
	}

	req.Header.Add("Content-Type", "application/json")

	client := &http.Client{}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	if res.StatusCode != http.StatusOK {
		return nil, errors.New(fmt.Sprintf("LLM API returned status code %d", res.StatusCode))
	}

	scanner := bufio.NewScanner(res.Body)

	//defer res.Body.Close()

	return scanner, nil
}

func ProcessLLMStream(
	channelId string, scanner *bufio.Scanner, chunkRate int,
	chunkFunction func(channelId string, chunk string, newMessage bool) error,
	endFunction func(channelId string, full string) error,
	ctrlChannel chan bool) (string, error) {

	fullMessage := ""
	currentMessage := ""
	chunkCount := 0
	newMessageFlag := false

	InfoLogger.Println("Processing LLM stream")
	for scanner.Scan() {
		// remove the "data: " prefix
		jsonChunkString := strings.Replace(scanner.Text(), "data: ", "", 1)

		// stop if the control channel is closed
		select {
		case <-ctrlChannel:
			InfoLogger.Println("LLM stream processing stopped")
			return "", nil
		default:
			// stop when we get the [DONE] message
			if jsonChunkString == "[DONE]" {
				err := endFunction(channelId, currentMessage)
				if err != nil {
					return "", err
				}
				break
			}

			// ignore empty lines
			if jsonChunkString == "" || jsonChunkString == "\n" || strings.Contains(jsonChunkString, ": pin") {
				continue
			}

			// parse json
			jsonParsed, err := gabs.ParseJSON([]byte(jsonChunkString))
			if err != nil {
				return "", err
			}

			// get chunk contents
			chunkDelta, ok := jsonParsed.Path("choices").Index(0).Path("delta").Data().(map[string]interface{})
			if !ok {
				return "", errors.New("LLM API returned invalid JSON")
			}

			// check if we have content key
			if chunkContent, ok := chunkDelta["content"]; ok {

				// add chunk to full message
				fullMessage += chunkContent.(string)
				currentMessage += chunkContent.(string)

				// increment chunk count
				chunkCount += 1

				if strings.Contains(currentMessage, "[[END]]") {
					err := endFunction(channelId, currentMessage)
					if err != nil {
						return "", err
					}

					// send confirmation message to the user
					var embeds []*discordgo.MessageEmbed
					embeds = append(embeds, &discordgo.MessageEmbed{
						Title:     "Session Ended: Bye!",
						Timestamp: time.Now().Format(time.RFC3339),
						Color:     15548997,
					})

					// send the embed message for the current channel
					_, err = s.ChannelMessageSendEmbeds(channelId, embeds)
					return "", err
				}

				// send out chunk updates every chunk_rate chunks
				if chunkCount%chunkRate == 0 && currentMessage != "" {

					newMessage := false
					// if we're sending our first set of chunks, set new_message to true
					if chunkCount == chunkRate || newMessageFlag == true {
						newMessage = true
						newMessageFlag = false
					}

					err := chunkFunction(channelId, currentMessage, newMessage)
					if err != nil {
						return "", err
					}

					// if new message has exceeded 2000 characters, reset it
					if len(currentMessage) > 2000 {
						currentMessage = ""
						newMessageFlag = true
					}
				}
			}
		}
	}

	InfoLogger.Println("LLM stream processing finished")

	return fullMessage, nil
}
