package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func processVideoForFastStart(filePath string) (string, error) {
	// create output file path .processing.mp4
	outputFilePath := filePath + ".processing.mp4"

	// create the ffmpeg command with -movflags +faststart to move moov atom to the beginning
	cmd := exec.Command("ffmpeg", "-i", filePath, "-c:v", "libx264", "-preset", "fast", "-c:a", "aac", "-b:a", "192k", "-movflags", "+faststart", "-shortest", outputFilePath)

	// run the command
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to run ffmpeg: %w", err)
	}

	return outputFilePath, nil
}

func getVideoAspectRatio(filePath string) (string, error) {
	// Create the ffprobe command
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)

	// Create a buffer to capture stdout
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	// Run the command
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to run ffprobe: %w", err)
	}

	// Define struct to unmarshal the JSON output
	type Stream struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	}

	type FFProbeOutput struct {
		Streams []Stream `json:"streams"`
	}

	// Unmarshal the JSON output
	var output FFProbeOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		return "", fmt.Errorf("failed to parse ffprobe output: %w", err)
	}

	// Check if we have at least one stream
	if len(output.Streams) == 0 {
		return "", fmt.Errorf("no video streams found")
	}

	// Get width and height from the first stream
	width := output.Streams[0].Width
	height := output.Streams[0].Height

	if width == 0 || height == 0 {
		return "", fmt.Errorf("invalid video dimensions: width=%d, height=%d", width, height)
	}

	// Calculate the aspect ratio
	ratio := float64(width) / float64(height)

	// Define tolerance for aspect ratio comparison (e.g., 1% tolerance)
	const tolerance = 0.01

	// Check for 16:9 (1.7778)
	if math.Abs(ratio-16.0/9.0) < tolerance {
		return "16:9", nil
	}

	// Check for 9:16 (0.5625)
	if math.Abs(ratio-9.0/16.0) < tolerance {
		return "9:16", nil
	}

	// If neither, return "other"
	return "other", nil
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	// Create a presign client from the S3 client
	presignClient := s3.NewPresignClient(s3Client)

	// Create the presigned request for GetObject
	presignedReq, err := presignClient.PresignGetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", fmt.Errorf("failed to presign request: %w", err)
	}

	// Return the URL from the presigned request
	return presignedReq.URL, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	// spilt videourl to get bucket / key tube-private-12345,portrait/vertical.mp4
	bucket := strings.Split(*(video.VideoURL), ",")[0]
	key := strings.Split(*(video.VideoURL), ",")[1]

	// generate presigned url
	presignedURL, err := generatePresignedURL(cfg.s3Client, bucket, key, 24*time.Hour)
	if err != nil {
		return database.Video{}, err
	}

	// return the video with presigned url
	video.VideoURL = &presignedURL
	return video, nil
}

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const maxMemory = 1 << 30
	r.ParseMultipartForm(maxMemory)

	// extract videoID from url path
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	fmt.Println("uploading video", videoID, "by user", userID)

	// TODO: implement the upload here
	videoFile, videoFileHeader, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't find video file", err)
		return
	}
	defer videoFile.Close()

	media_type, _, err := mime.ParseMediaType(videoFileHeader.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid video file type", err)
		return
	}

	if media_type != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid video file type", nil)
		return
	}

	// get the video metadata from database and check owner
	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video", err)
		return
	}

	// check if the video is owned by the user
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You are not authorized to upload a video for this user", nil)
		return
	}

	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create temp file", err)
		return
	}

	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	// copy video to temp file
	if _, err := io.Copy(tempFile, videoFile); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't copy video file", err)
		return
	}

	// create processed version of video
	processedFilePath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't process video", err)
		return
	}
	defer os.Remove(processedFilePath)

	// get the ratio from processed file
	aspectRatio, err := getVideoAspectRatio(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video aspect ratio", err)
		return
	}
	filePrefix := ""
	switch aspectRatio {
	case "16:9":
		filePrefix = "landscape"
	case "9:16":
		filePrefix = "portrait"
	default:
		filePrefix = "other"
	}

	// open the processed file for upload
	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't open processed file", err)
		return
	}
	defer processedFile.Close()

	// build file key using base64
	fileKey := make([]byte, 16)
	if _, err := rand.Read(fileKey); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate file key", err)
		return
	}
	fileKeyString := base64.RawURLEncoding.EncodeToString(fileKey) + ".mp4"

	// upload processed video to S3
	_, err = cfg.s3Client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(filePrefix + "/" + fileKeyString),
		Body:        processedFile,
		ContentType: aws.String(media_type),
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't upload processed file", err)
		return
	}

	// video url format https://<bucket-name>.s3.<region>.amazonaws.com/<key>
	// videoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s/%s", cfg.s3Bucket, cfg.s3Region, filePrefix, fileKeyString)
	// tube-private-12345,portrait/vertical.mp4
	videoURL := fmt.Sprintf("%s,%s/%s", cfg.s3Bucket, filePrefix, fileKeyString)
	fmt.Println("videoURL", videoURL)

	video.VideoURL = &videoURL
	cfg.db.UpdateVideo(video)

	// get the signed video
	signedVideo, err := cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get signed video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, signedVideo)

	// respondWithJSON(w, http.StatusOK, database.Video{
	// 	ID:           video.ID,
	// 	VideoURL:     video.VideoURL,
	// 	CreatedAt:    video.CreatedAt,
	// 	UpdatedAt:    video.UpdatedAt,
	// 	ThumbnailURL: video.ThumbnailURL,
	// })

}
