package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {

	// Set limit for upload to 1 GB
	const uploadLimit = 1 << 30

	// Set http body with upload limit
	r.Body = http.MaxBytesReader(w, r.Body, uploadLimit)

	// Extract the videoID from the URL path parameters and parse it as a UUID
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	// Authenticate the user to get a userID
	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	// Validate JWT
	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	// Get the video metadata from the database
	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't find video", err)
		return
	}

	// Check if user is the owner of the video
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Not authorized to update this video", nil)
		return
	}

	// Parse the uploaded video file from the form data
	file, handler, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	// Validate the uploaded file to ensure it's an MP4 video
	mediaType, _, err := mime.ParseMediaType(handler.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid Content-Type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid file type, only MP4 is allowed", nil)
		return
	}

	// Save the uploaded file to a temporary file on disk
	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not create temp file", err)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	if _, err := io.Copy(tempFile, file); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not write file to disk", err)
		return
	}

	// Reset the tempFile's file pointer to the beginning
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not reset file pointer", err)
		return
	}

	// Determine aspect ratio of video from tempFile
	directory := ""
	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error determining aspect ratio", err)
		return
	}

	// Switch statement for specific aspect ratio
	switch aspectRatio {
	case "16:9":
		directory = "landscape"
	case "9:16":
		directory = "portrait"
	default:
		directory = "other"
	}


	// Setup key for video file
	key := getAssetPath(mediaType)
	key = filepath.Join(directory, key)

	// Get Processed file path for video file
	processedFilePath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error processing video", err)
		return
	}
	defer os.Remove(processedFilePath)

	// Open processed file path
	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not open processed file", err)
		return
	}
	defer processedFile.Close()

	// Put the object into S3
	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:			aws.String(cfg.s3Bucket),
		Key:			aws.String(key),
		Body:			processedFile,
		ContentType:	aws.String(mediaType),
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error uploading file to S3", err)
		return
	}

	// Update the VideoURL of the video record in the database with the S3 bucket and key
	url := fmt.Sprintf("%s,%s", cfg.s3Bucket, key)
	video.VideoURL = &url
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}

	// Generate a presigned URL for the video object
	video, err = cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate presigned URL", err)
		return
	}

	// Respond with data in JSON format
	respondWithJSON(w, http.StatusOK, video)
}

// Function to get aspect ratio from provided filepath
func getVideoAspectRatio(filePath string) (string, error) {

	// Run ffprobe command with file path argument
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_streams",
		filePath,
	)

	// Set exec.Cmd's Stdout field to a pointer to a new bytes.Buffer
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	// Run the command
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffprobe error: %v", err)
	}

	// Unmarshal stdout of the command into a JSON struct for width and height
	var output struct {
		Streams []struct {
			Width	int `json:"width"`
			Height	int `json:"height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		return "", fmt.Errorf("could not parse ffprobe output: %v", err)
	}

	// Check to ensure video stream is found
	if len(output.Streams) == 0 {
		return "", errors.New("no video streams found")
	}

	// Perform calculations to determine aspect ratio
	width := output.Streams[0].Width
	height := output.Streams[0].Height

	if width == 16 * height / 9 {
		return "16:9", nil
	} else if height == 16 * width / 9 {
		return "9:16", nil
	}
	
	return "other", nil
}

// Function to setup "fast start" for processing videos
func processVideoForFastStart(inputFilePath string) (string, error) {

	// String for the output filepath
	processedFilePath := fmt.Sprintf("%s.processing", inputFilePath)

	// Run command for ffmpeg
	cmd := exec.Command("ffmpeg",
		"-i", inputFilePath,
		"-movflags", "faststart",
		"-codec", "copy",
		"-f", "mp4",
		processedFilePath,
	)

	// Set exec.Cmd's Stderr field to a pointer to a new bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	// Run the command
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("error processing video: %s, %v", stderr.String(), err)
	}

	// Get file info via os.Stat
	fileInfo, err := os.Stat(processedFilePath)
	if err != nil {
		return "", fmt.Errorf("could not stat processed file: %v", err)
	}
	
	// Check processed file is not empty
	if fileInfo.Size() == 0 {
		return "", fmt.Errorf("processed file is empty")
	}

	return processedFilePath, nil
}

// Function to take video object input and return object with VideoURL set to presigned URL
func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {

	// Check if video URL exists
	if video.VideoURL == nil {
		return video, nil
	}

	// Get bucket and key data for video object
	parts := strings.Split(*video.VideoURL, ",")
	if len(parts) < 2 {
		return video, nil
	}
	bucket := parts[0]
	key := parts[1]

	// Generate a presigned URL for the video object
	presigned, err := generatePresignedURL(cfg.s3Client, bucket, key, 5*time.Minute)
	if err != nil {
		return video, err
	}

	// Set the VideoURL field of the video object to presigned and return updated video
	video.VideoURL = &presigned
	return video, nil
}

// Function to generate a presigned URL
func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {

	// Use SDK to create a s3.PresignClient
	presignClient := s3.NewPresignClient(s3Client)

	// Generate presigned URL
	presignedUrl, err := presignClient.PresignGetObject(context.TODO(), &s3.GetObjectInput{
		Bucket:		aws.String(bucket),
		Key:		aws.String(key),
	}, s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", fmt.Errorf("failed to generate presigned URL: %v", err)
	}

	return presignedUrl.URL, nil
}