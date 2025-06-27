package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

func (cfg apiConfig) ensureAssetsDir() error {
	if _, err := os.Stat(cfg.assetsRoot); os.IsNotExist(err) {
		return os.Mkdir(cfg.assetsRoot, 0755)
	}
	return nil
}


// Function to get the asset file path
func getAssetPath(videoID uuid.UUID, mediaType string) string {

	// Get the extension of mediaType
	ext := mediaTypeToExt(mediaType)
	return fmt.Sprintf("%s%s", videoID, ext)
}

// Function to get asset disk path
func (cfg apiConfig) getAssetDiskPath(assetPath string) string {
	
	// Join the root path with the file path
	return filepath.Join(cfg.assetsRoot, assetPath)
}

// Function to get asset URL
func (cfg apiConfig) getAssetURL(assetPath string) string {

	// Format a string to the specified port and full asset disk path
	return fmt.Sprintf("http://localhost:%s/assets/%s", cfg.port, assetPath)
}

// Function to gather mediaType's particular extension
func mediaTypeToExt(mediaType string) string {

	// Split mediaType into separate parts
	parts := strings.Split(mediaType, "/")

	// Check if there are more than 2 parts
	if len(parts) != 2 {
		return ".bin"
	}

	// Return last part of string with "." as prefix
	return "." + parts[1]
}