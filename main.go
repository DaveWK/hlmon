package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/PagerDuty/go-pagerduty"
	"github.com/slack-go/slack"
)

type Config struct {
	SlackToken         string `toml:"slack_token"`
	SlackChannel       string `toml:"slack_channel"`
	PagerDutyAPIKey    string `toml:"pagerduty_api_key"`
	PagerDutyServiceID string `toml:"pagerduty_service_id"`
	BasePath           string `toml:"base_path"`
	ValidatorAddress   string `toml:"validator_address"`
	CheckInterval      int    `toml:"check_interval"`
}

type ValidatorData struct {
	HomeValidator              string                     `json:"home_validator"`
	ValidatorsMissingHeartbeat []string                   `json:"validators_missing_heartbeat"`
	HeartbeatStatuses          map[string]HeartbeatStatus `json:"heartbeat_statuses"`
}

type HeartbeatStatus struct {
	SinceLastSuccess float64  `json:"since_last_success"`
	LastAckDuration  *float64 `json:"last_ack_duration"`
}

type LogArrayEntry struct {
	Timestamp string        `json:"timestamp"`
	Validator ValidatorData `json:"validator_data"`
}

func sendSlackAlert(api *slack.Client, channel, message string) {
	_, _, err := api.PostMessage(
		channel,
		slack.MsgOptionText(message, false),
	)
	if err != nil {
		log.Printf("Slack API Error: %s\n", err)
	}
}

func sendPagerDutyAlert(routingKey, description string) {
	event := pagerduty.V2Event{
		RoutingKey: routingKey,
		Action:     "trigger",
		Payload: &pagerduty.V2Payload{
			Summary:   description,
			Source:    "validator-monitoring-script",
			Severity:  "critical",
			Component: "Validator Monitoring",
		},
	}
	_, err := pagerduty.ManageEventWithContext(context.Background(), event)
	if err != nil {
		log.Printf("PagerDuty API Error: %s\n", err)
	}
}
func (vd *ValidatorData) UnmarshalJSON(data []byte) error {
	// Create a temporary struct for the standard fields
	type Alias ValidatorData
	aux := &struct {
		HeartbeatStatuses [][]interface{} `json:"heartbeat_statuses"`
		*Alias
	}{
		Alias: (*Alias)(vd),
	}

	// Unmarshal the JSON into the auxiliary structure
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	// Convert HeartbeatStatuses from array to map
	vd.HeartbeatStatuses = make(map[string]HeartbeatStatus)
	for _, entry := range aux.HeartbeatStatuses {
		if len(entry) != 2 {
			continue
		}

		key, ok := entry[0].(string)
		if !ok {
			continue
		}

		valueBytes, err := json.Marshal(entry[1])
		if err != nil {
			continue
		}

		var heartbeatStatus HeartbeatStatus
		if err := json.Unmarshal(valueBytes, &heartbeatStatus); err != nil {
			continue
		}

		vd.HeartbeatStatuses[key] = heartbeatStatus
	}

	return nil
}
func main() {
	var config Config
	if _, err := toml.DecodeFile("config.toml", &config); err != nil {
		log.Fatalf("Error loading configuration: %s\n", err)
	}

	slackClient := slack.New(config.SlackToken)

	latestLogFile, err := findLatestLogFile(config.BasePath)
	if err != nil {
		log.Fatalf("Failed to find latest log file: %v", err)
	}
	println(latestLogFile)
	for {
		file, err := os.Open(latestLogFile)
		if err != nil {
			log.Printf("Error opening log file: %s\n", err)
			time.Sleep(30 * time.Second)
			continue
		}

		decoder := json.NewDecoder(file)
		var lastRawEntry json.RawMessage
		for {
			var rawEntry json.RawMessage
			if err := decoder.Decode(&rawEntry); err != nil {
				if err.Error() == "EOF" {
					break
				}
				log.Printf("Error decoding JSON line: %s\n", err)
				continue
			}
			lastRawEntry = rawEntry
		}

		if lastRawEntry != nil {
			log.Printf("Raw JSON content: %s\n", string(lastRawEntry))

			// Attempt to unmarshal as an array containing a timestamp and data
			var logArray []interface{}
			if err := json.Unmarshal(lastRawEntry, &logArray); err == nil && len(logArray) == 2 {
				// Get only the last element
				timestamp, ok := logArray[0].(string)
				if !ok {
					log.Printf("Error: Expected timestamp as first element, got: %v", logArray[0])
					continue
				}

				validatorDataBytes, err := json.Marshal(logArray[1])
				if err != nil {
					log.Printf("Error marshaling validator data: %s", err)
					continue
				}

				var validatorData ValidatorData
				if err := json.Unmarshal(validatorDataBytes, &validatorData); err != nil {
					log.Printf("Error decoding validator data: %s", err)
					continue
				}

				// Create the log entry with the last element
				logEntry := LogArrayEntry{
					Timestamp: timestamp,
					Validator: validatorData,
				}

				// Process the last log entry only
				processLogEntry(logEntry, slackClient, config)
			} else {
				log.Printf("Error: Could not unmarshal JSON line as expected array")
			}
		}

		file.Close()
		time.Sleep(time.Duration(config.CheckInterval) * time.Second)
	}
}

func processLogEntry(logEntry LogArrayEntry, slackClient *slack.Client, config Config) {
	log.Printf("Timestamp: %s\n", logEntry.Timestamp)
	if status, found := logEntry.Validator.HeartbeatStatuses[config.ValidatorAddress]; found {
		if status.SinceLastSuccess > 40 || (status.LastAckDuration != nil && *status.LastAckDuration > 0.02) || status.LastAckDuration == nil {
			alertMessage := fmt.Sprintf("Alert for HyperLiq validator %s:\nsince_last_success = %v, last_ack_duration = %v", config.ValidatorAddress, status.SinceLastSuccess, status.LastAckDuration)
			sendSlackAlert(slackClient, config.SlackChannel, alertMessage)
			sendPagerDutyAlert(config.PagerDutyAPIKey, alertMessage)
		}
	} else if status.SinceLastSuccess <= 0 || *status.LastAckDuration <= 0 {
		alertMessage := fmt.Sprintf("Alert for HyperLiq validator %s:\nsince_last_success = %v, last_ack_duration = %v", config.ValidatorAddress, status.SinceLastSuccess, status.LastAckDuration)
		sendSlackAlert(slackClient, config.SlackChannel, alertMessage)
		sendPagerDutyAlert(config.PagerDutyAPIKey, alertMessage)
	}
}

func findLatestLogFile(basePath string) (string, error) {
	latestDateDir, err := findLatestDir(basePath)
	if err != nil {
		return "", fmt.Errorf("failed to find latest date directory: %w", err)
	}

	latestLogFile, err := findLatestFile(latestDateDir)
	if err != nil {
		return "", fmt.Errorf("failed to find latest log file: %w", err)
	}

	return latestLogFile, nil
}

func findLatestDir(basePath string) (string, error) {
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return "", err
	}

	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, entry.Name())
		}
	}

	if len(dirs) == 0 {
		return "", fmt.Errorf("no directories found in %s", basePath)
	}

	latestDir := dirs[0]
	for _, dir := range dirs {
		if dir > latestDir {
			latestDir = dir
		}
	}

	return fmt.Sprintf("%s/%s", basePath, latestDir), nil
}
func findLatestFile(dirPath string) (string, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return "", err
	}

	var files []string
	for _, entry := range entries {
		if !entry.IsDir() {
			files = append(files, entry.Name())
		}
	}

	if len(files) == 0 {
		return "", fmt.Errorf("no files found in %s", dirPath)
	}

	// Sort files to ensure correct order
	sort.Slice(files, func(i, j int) bool {
		iInt, errI := strconv.Atoi(files[i])
		jInt, errJ := strconv.Atoi(files[j])
		if errI == nil && errJ == nil {
			return iInt < jInt
		}
		return files[i] < files[j]
	})

	latestFile := files[len(files)-1]

	return fmt.Sprintf("%s/%s", dirPath, latestFile), nil
}
