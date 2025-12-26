package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"crypto/rand"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadThumbnail(w http.ResponseWriter, r *http.Request) {
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

	fmt.Println("uploading thumbnail for video", videoID, "by user", userID)

	// TODO: implement the upload here
	const maxMemory = 10 << 20
	r.ParseMultipartForm(maxMemory)

	thumbnailFile, thumbnailFileHeader, err := r.FormFile("thumbnail")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't find thumbnail file", err)
		return
	}
	defer thumbnailFile.Close()

	// media_type := thumbnailFileHeader.Header.Get("Content-Type")
	media_type, _, err := mime.ParseMediaType(thumbnailFileHeader.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid thumbnail file type", err)
		return
	}
	if media_type != "image/png" && media_type != "image/jpeg" {
		respondWithError(w, http.StatusBadRequest, "Invalid thumbnail file type", nil)
		return
	}

	// thumbnailBytes, err := io.ReadAll(thumbnailFile)
	// if err != nil {
	// 	respondWithError(w, http.StatusInternalServerError, "Couldn't read thumbnail file", err)
	// 	return
	// }

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video", err)
		return
	}

	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You are not authorized to upload a thumbnail for this video", nil)
		return
	}

	// use crypto/rand.Read / base64.RawURLEncoding to generate a random string
	thumbnailID := make([]byte, 16)
	if _, err := rand.Read(thumbnailID); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate thumbnail ID", err)
		return
	}
	thumbnailIDString := base64.RawURLEncoding.EncodeToString(thumbnailID)
	imagePath := filepath.Join(cfg.assetsRoot, thumbnailIDString+"."+strings.TrimPrefix(media_type, "image/"))
	fmt.Println("imagepatttttthhhhh", imagePath)
	imageFile, err := os.Create(imagePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create thumbnail file", err)
		return
	}
	defer imageFile.Close()
	io.Copy(imageFile, thumbnailFile)

	thumbnailURL := fmt.Sprintf("http://localhost:8091/assets/%s.%s", thumbnailIDString, strings.TrimPrefix(media_type, "image/"))
	fmt.Println("thumbnailURL", thumbnailURL)

	video.ThumbnailURL = &thumbnailURL
	cfg.db.UpdateVideo(video)

	respondWithJSON(w, http.StatusOK, database.Video{
		ID:           video.ID,
		ThumbnailURL: video.ThumbnailURL,
		CreatedAt:    video.CreatedAt,
		UpdatedAt:    video.UpdatedAt,
		VideoURL:     video.VideoURL,
	})
}
