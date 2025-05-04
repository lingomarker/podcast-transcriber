package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	// "github.com/google/generative-ai-go/genai"
	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3" // Import for side effects (driver registration)
	"google.golang.org/genai"
	// "google.golang.org/api/option"
)

// Config holds application configuration
type Config struct {
	GeminiAPIKey string `json:"GeminiAPIKey"`
	DatabasePath string `json:"DatabasePath"`
	UploadDir    string `json:"UploadDir"`
	ServerPort   string `json:"ServerPort"`
}

// PodcastEntry represents a record in the database
type PodcastEntry struct {
	ID                   string
	Filename             string
	StoragePath          string
	Description          string
	OriginalTranscript   string
	GeminiTranscriptJSON sql.NullString // Use NullString for nullable JSON field
	UploadTime           time.Time
	Status               string // e.g., "uploaded", "transcribing", "completed", "failed"
}

var (
	cfg *Config
	db  *sql.DB
)

func loadConfig(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open config file: %w", err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	config := &Config{}
	err = decoder.Decode(config)
	if err != nil {
		return nil, fmt.Errorf("failed to decode config file: %w", err)
	}
	return config, nil
}

func initDB(dbPath string) error {
	var err error
	db, err = sql.Open("sqlite3", dbPath)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}

	// Ping the database to ensure the connection is open
	if err = db.Ping(); err != nil {
		db.Close() // Close the failed connection
		return fmt.Errorf("failed to connect to database: %w", err)
	}

	// Create the table if it doesn't exist
	createTableSQL := `CREATE TABLE IF NOT EXISTS podcasts (
		id TEXT PRIMARY KEY,
		filename TEXT NOT NULL,
		storage_path TEXT NOT NULL UNIQUE,
		description TEXT,
		original_transcript TEXT,
		gemini_transcript_json TEXT,
		upload_time DATETIME NOT NULL,
		status TEXT NOT NULL
	);`

	_, err = db.Exec(createTableSQL)
	if err != nil {
		db.Close() // Close connection on failure
		return fmt.Errorf("failed to create table: %w", err)
	}

	log.Println("Database initialized successfully.")
	return nil
}

func createPodcastEntry(entry *PodcastEntry) error {
	insertSQL := `INSERT INTO podcasts 
		(id, filename, storage_path, description, original_transcript, upload_time, status) 
		VALUES (?, ?, ?, ?, ?, ?, ?)`

	_, err := db.Exec(
		insertSQL,
		entry.ID,
		entry.Filename,
		entry.StoragePath,
		entry.Description,
		entry.OriginalTranscript,
		entry.UploadTime,
		entry.Status,
	)
	if err != nil {
		return fmt.Errorf("failed to insert podcast entry: %w", err)
	}
	return nil
}

func updatePodcastTranscript(id string, geminiJSON string, status string) error {
	updateSQL := `UPDATE podcasts 
		SET gemini_transcript_json = ?, status = ? 
		WHERE id = ?`

	_, err := db.Exec(updateSQL, geminiJSON, status, id)
	if err != nil {
		return fmt.Errorf("failed to update podcast transcript: %w", err)
	}
	return nil
}

func handleUploadPodcast(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Only POST method is allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse the multipart form data
	// 32MB is the default maxMemory, can be adjusted
	err := r.ParseMultipartForm(32 << 20)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to parse multipart form: %v", err), http.StatusBadRequest)
		return
	}

	// Get the audio file
	file, handler, err := r.FormFile("audio")
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get audio file: %v", err), http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Get other fields
	description := r.FormValue("description")
	originalTranscript := r.FormValue("original_transcript")

	// Generate a unique ID
	podcastID := uuid.New().String()

	// Define storage path for the audio file
	err = os.MkdirAll(cfg.UploadDir, 0755) // Ensure upload directory exists
	if err != nil {
		log.Printf("Error creating upload directory %s: %v", cfg.UploadDir, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	// Use the generated UUID as the filename to avoid conflicts and improve security
	audioFileName := podcastID + filepath.Ext(handler.Filename)
	storagePath := filepath.Join(cfg.UploadDir, audioFileName)

	// Save the uploaded file
	dst, err := os.Create(storagePath)
	if err != nil {
		log.Printf("Error saving file %s: %v", storagePath, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	defer dst.Close()

	if _, err := io.Copy(dst, file); err != nil {
		log.Printf("Error copying file content to %s: %v", storagePath, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Create initial database entry
	entry := &PodcastEntry{
		ID:                 podcastID,
		Filename:           handler.Filename,
		StoragePath:        storagePath,
		Description:        description,
		OriginalTranscript: originalTranscript,
		UploadTime:         time.Now(),
		Status:             "uploaded",
	}

	err = createPodcastEntry(entry)
	if err != nil {
		log.Printf("Error creating database entry: %v", err)
		// Clean up the saved file on DB error
		os.Remove(storagePath)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// --- Call Gemini API for Transcription (Synchronous for simplicity) ---
	// In a real application, this would likely be a background task.

	log.Printf("Starting Gemini transcription for podcast ID: %s", podcastID)

	geminiTranscript, err := transcribeAudioWithGemini(storagePath, description, originalTranscript, cfg.GeminiAPIKey)
	if err != nil {
		log.Printf("Error during Gemini transcription for ID %s: %v", podcastID, err)
		// Update status to failed
		updateErr := updatePodcastTranscript(podcastID, "", "failed")
		if updateErr != nil {
			log.Printf("Error updating status to failed for ID %s: %v", podcastID, updateErr)
		}
		http.Error(w, fmt.Sprintf("Transcription failed: %v", err), http.StatusInternalServerError)
		return
	}

	// Update database with Gemini transcript
	err = updatePodcastTranscript(podcastID, geminiTranscript, "completed")
	if err != nil {
		log.Printf("Error updating database with Gemini transcript for ID %s: %v", podcastID, err)
		http.Error(w, "Transcription completed but failed to save result", http.StatusInternalServerError)
		return
	}

	log.Printf("Transcription completed and saved for podcast ID: %s", podcastID)

	// Respond with success
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{
		"id":      podcastID,
		"status":  "completed",
		"message": "Podcast uploaded and transcribed successfully",
	})
}

// transcribeAudioWithGemini calls the Gemini API to transcribe audio.
// It sends the audio file along with description and original transcript
// to the Gemini 2.0 Flash model and asks for the transcript in JSON format.
func transcribeAudioWithGemini(audioFilePath, description, originalTranscript, apiKey string) (string, error) {
	ctx := context.Background()

	client, _ := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})

	localAudioPath := audioFilePath
	uploadedFile, _ := client.Files.UploadFromPath(
		ctx,
		localAudioPath,
		nil,
	)

	parts := []*genai.Part{
		genai.NewPartFromText(fmt.Sprintf(`Transcribe the following audio from a podcast episode.
		Generate the transcript in JSON format with speaker diarization and timestamps.
		Use the following podcast description for context: `+"```"+`%s`+"```"+`.
		Use the following original transcript as a reference: `+"```"+`%s`+"```"+`.
		The audio may contain advertisements that are typically not included in the original transcript.
		Merge the ad sections (from the generated transcript) back into the original transcript, ensuring correct timestamps.
		Format the final output as a JSON array of objects, where each object contains speaker, timestamp, and text.`, description, originalTranscript)),
		genai.NewPartFromURI(uploadedFile.URI, uploadedFile.MIMEType),
	}
	contents := []*genai.Content{
		genai.NewContentFromParts(parts, genai.RoleUser),
	}

	result, _ := client.Models.GenerateContent(
		ctx,
		"gemini-2.0-flash",
		contents,
		nil,
	)

	var geminiText string
	if result != nil && len(result.Text()) > 0 {
		geminiText = result.Text()
	} else {
		return "", fmt.Errorf("gemini API returned empty response")
	}

	// Simple attempt to extract potential JSON block (might need more robust parsing)
	// Look for the start and end of the JSON array
	jsonStart := findJSONStart(geminiText)
	jsonEnd := findJSONEnd(geminiText)

	if jsonStart == -1 || jsonEnd == -1 || jsonEnd < jsonStart {
		log.Printf("Warning: Could not find valid JSON block in Gemini response: %s", geminiText)
		// As a fallback, return the raw text response wrapped in a simple JSON structure
		fallbackJSON, _ := json.Marshal(map[string]string{"raw_text": geminiText})
		return string(fallbackJSON), fmt.Errorf("could not extract JSON from gemini response, returning raw text fallback")
	}

	extractedJSON := geminiText[jsonStart : jsonEnd+1]

	// Optional: Validate the extracted JSON (basic check)
	var temp []map[string]interface{}
	if err := json.Unmarshal([]byte(extractedJSON), &temp); err != nil {
		log.Printf("Warning: Extracted text is not valid JSON, returning raw text fallback. Extracted: %s Error: %v", extractedJSON, err)
		fallbackJSON, _ := json.Marshal(map[string]string{"raw_text": geminiText})
		return string(fallbackJSON), fmt.Errorf("extracted JSON is invalid, returning raw text fallback")
	}

	return extractedJSON, nil
}

// findJSONStart attempts to find the index of the start of a JSON array.
func findJSONStart(text string) int {
	// Find the first occurrence of '[' not preceded by text that looks like markdown code block start
	// This is a simplified heuristic. A real parser would be better.
	idx := 0
	for idx < len(text) {
		if text[idx] == '[' {
			// Simple check to avoid markdown code blocks like ```json
			if idx >= 3 && text[idx-3:idx] == "```" {
				idx++ // Skip past this potential false positive
				continue
			}
			return idx
		}
		idx++
	}
	return -1
}

// findJSONEnd attempts to find the index of the end of a JSON array.
func findJSONEnd(text string) int {
	// Find the last occurrence of ']'
	idx := len(text) - 1
	for idx >= 0 {
		if text[idx] == ']' {
			// Simple check to avoid markdown code blocks like ```
			if idx+3 <= len(text) && text[idx+1:idx+4] == "```" {
				idx-- // Skip backwards
				continue
			}
			return idx
		}
		idx--
	}
	return -1
}

func main() {
	var err error
	cfg, err = loadConfig("./config.json")
	if err != nil {
		log.Fatalf("Error loading config: %v", err)
	}

	err = initDB(cfg.DatabasePath)
	if err != nil {
		log.Fatalf("Error initializing database: %v", err)
	}
	defer db.Close()

	// Ensure upload directory exists
	err = os.MkdirAll(cfg.UploadDir, 0755)
	if err != nil {
		log.Fatalf("Error creating upload directory %s: %v", cfg.UploadDir, err)
	}

	log.Printf("Starting server on %s...", cfg.ServerPort)
	http.HandleFunc("/upload-podcast", handleUploadPodcast)
	log.Fatal(http.ListenAndServe(cfg.ServerPort, nil))
}
