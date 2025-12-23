package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

const (
	receiveDir     = "received"       // Directory where received files are stored
	port           = ":8000"          // Port for network communication
	torProxyAddr   = "127.0.0.1:9050" // Tor proxy address
	chunkSize      = 32 * 1024        // 32KB chunks for better performance
	maxFilenameLen = 255              // Maximum filename length
)

var (
	progressWriter *ProgressWriter // Global progress tracker
	startTime      time.Time       // Start time for transfers
)

// FileInfo contains metadata about the file being transferred
type FileInfo struct {
	Filename    string // Name of the file
	FileSize    int64  // Size of the file in bytes
	IsDirectory bool   // Whether this is a directory
}

// ProgressWriter tracks and displays transfer progress
type ProgressWriter struct {
	total      int64     // Total bytes to transfer
	current    int64     // Current bytes transferred
	lastPrint  int64     // Last printed byte count (for throttling updates)
	startTime  time.Time // Start time of transfer
	filename   string    // Name of the file being transferred
}

// NewProgressWriter creates a new progress tracker
func NewProgressWriter(total int64, filename string) *ProgressWriter {
	return &ProgressWriter{
		total:     total,
		startTime: time.Now(),
		filename:  filename,
	}
}

// Write implements io.Writer interface and tracks bytes written
func (pw *ProgressWriter) Write(p []byte) (int, error) {
	n := len(p)
	pw.current += int64(n)
	pw.printProgress()
	return n, nil
}

// printProgress displays transfer progress without ETA
func (pw *ProgressWriter) printProgress() {
	now := time.Now()
	elapsed := now.Sub(pw.startTime)
	
	// Update only every 100ms to avoid console spam
	if pw.current-pw.lastPrint < 1024 && elapsed.Milliseconds()%100 != 0 {
		return
	}
	
	var percent float64
	if pw.total > 0 {
		percent = float64(pw.current) / float64(pw.total) * 100
	}
	
	// Calculate transfer speed
	speed := float64(pw.current) / elapsed.Seconds()
	speedStr := formatBytes(speed) + "/s"
	
	// Display progress information without ETA
	if pw.total > 0 {
		// When we're at or near 100%, show 100% exactly
		if pw.current >= pw.total {
			percent = 100.0
			pw.current = pw.total // Ensure we don't exceed total
			fmt.Printf("\r%s: %s/%s (100.0%%) | Speed: %s",
				pw.filename,
				formatBytes(float64(pw.current)),
				formatBytes(float64(pw.total)),
				speedStr)
		} else {
			fmt.Printf("\r%s: %s/%s (%.1f%%) | Speed: %s",
				pw.filename,
				formatBytes(float64(pw.current)),
				formatBytes(float64(pw.total)),
				percent,
				speedStr)
		}
	} else {
		// Unknown total size
		fmt.Printf("\r%s: %s | Speed: %s",
			pw.filename,
			formatBytes(float64(pw.current)),
			speedStr)
	}
	
	pw.lastPrint = pw.current
}

// Finalize prints the final transfer summary
func (pw *ProgressWriter) Finalize() {
	elapsed := time.Since(pw.startTime)
	speed := float64(pw.current) / elapsed.Seconds()
	
	// Ensure we show 100% for the final display
	if pw.total > 0 {
		pw.current = pw.total
		fmt.Printf("\r%s: %s transferred in %s (%.1f %s/s)        \n",
			pw.filename,
			formatBytes(float64(pw.current)),
			formatDuration(elapsed),
			speed/1024/1024, "MB")
	} else {
		fmt.Printf("\r%s: %s transferred in %s (%.1f %s/s)         ",
			pw.filename,
			formatBytes(float64(pw.current)),
			formatDuration(elapsed),
			speed/1024/1024, "MB")
	}
}

// formatBytes converts bytes to human-readable format
func formatBytes(b float64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for b >= 1024 && i < len(units)-1 {
		b /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", b, units[i])
}

// formatDuration formats duration in HH:MM:SS or MM:SS format
func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	
	if h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// sendFileInfo sends file metadata to the receiver
func sendFileInfo(conn net.Conn, fi FileInfo) error {
	// Send filename length (4 bytes)
	filenameBytes := []byte(fi.Filename)
	filenameLen := uint32(len(filenameBytes))
	if err := binary.Write(conn, binary.BigEndian, filenameLen); err != nil {
		return err
	}
	
	// Send filename
	if _, err := conn.Write(filenameBytes); err != nil {
		return err
	}
	
	// Send file size (8 bytes)
	if err := binary.Write(conn, binary.BigEndian, fi.FileSize); err != nil {
		return err
	}
	
	// Send directory flag (1 byte)
	isDir := byte(0)
	if fi.IsDirectory {
		isDir = 1
	}
	if _, err := conn.Write([]byte{isDir}); err != nil {
		return err
	}
	
	return nil
}

// receiveFileInfo receives file metadata from the sender
func receiveFileInfo(conn net.Conn) (*FileInfo, error) {
	// Read filename length
	var filenameLen uint32
	if err := binary.Read(conn, binary.BigEndian, &filenameLen); err != nil {
		return nil, err
	}
	
	// Validate filename length
	if filenameLen > maxFilenameLen {
		return nil, fmt.Errorf("filename too long: %d bytes", filenameLen)
	}
	
	// Read filename
	filenameBytes := make([]byte, filenameLen)
	if _, err := io.ReadFull(conn, filenameBytes); err != nil {
		return nil, err
	}
	filename := string(filenameBytes)
	
	// Read file size
	var fileSize int64
	if err := binary.Read(conn, binary.BigEndian, &fileSize); err != nil {
		return nil, err
	}
	
	// Read directory flag
	dirFlag := make([]byte, 1)
	if _, err := io.ReadFull(conn, dirFlag); err != nil {
		return nil, err
	}
	isDirectory := dirFlag[0] == 1
	
	return &FileInfo{
		Filename:    filename,
		FileSize:    fileSize,
		IsDirectory: isDirectory,
	}, nil
}

// sendSingleFile sends a single file with custom filename
func sendSingleFile(conn net.Conn, filePath string, filename string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()
	
	fileInfo, err := file.Stat()
	if err != nil {
		return err
	}
	
	// Send file metadata with custom filename (could be relative path)
	fi := FileInfo{
		Filename:    filename,
		FileSize:    fileInfo.Size(),
		IsDirectory: false,
	}
	
	if err := sendFileInfo(conn, fi); err != nil {
		return fmt.Errorf("failed to send file info: %w", err)
	}
	
	// Create progress tracker
	progressWriter = NewProgressWriter(fi.FileSize, filepath.Base(filename))
	defer progressWriter.Finalize()
	
	fmt.Printf("  Sending %s (%s)...\n", filename, formatBytes(float64(fi.FileSize)))
	
	// Send file data
	buffer := make([]byte, chunkSize)
	totalSent := int64(0)
	
	for {
		n, err := file.Read(buffer)
		if err != nil && err != io.EOF {
			return err
		}
		
		if n == 0 {
			break
		}
		
		if _, err := conn.Write(buffer[:n]); err != nil {
			return err
		}
		
		totalSent += int64(n)
		progressWriter.current = totalSent
		progressWriter.printProgress()
		
		if err == io.EOF {
			break
		}
	}
	
	progressWriter.current = fi.FileSize
	progressWriter.printProgress()
	
	return nil
}

// sendDirectoryRaw sends a directory recursively
func sendDirectoryRaw(conn net.Conn, dirPath string) error {
	// Get the absolute path to calculate relative paths correctly
	absDirPath, err := filepath.Abs(dirPath)
	if err != nil {
		return err
	}
	
	// Send the root directory marker
	rootDirInfo := FileInfo{
		Filename:    filepath.Base(dirPath),
		FileSize:    0,
		IsDirectory: true,
	}
	
	if err := sendFileInfo(conn, rootDirInfo); err != nil {
		return fmt.Errorf("failed to send root directory info: %w", err)
	}
	
	fmt.Printf("Sending directory: %s\n", rootDirInfo.Filename)
	
	// Walk through the directory
	return filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		
		// Skip the root directory itself (we already sent it)
		if path == dirPath {
			return nil
		}
		
		// Get absolute path for this item
		absPath, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		
		// Calculate relative path from the directory being sent
		relPath, err := filepath.Rel(absDirPath, absPath)
		if err != nil {
			return err
		}
		
		// Prepend the directory name to the relative path
		// Wenn wir oc/ senden, sollte config.json als oc/config.json gesendet werden
		baseName := filepath.Base(absDirPath)
		fullRelPath := filepath.Join(baseName, relPath)
		
		// Convert to forward slashes for consistency
		fullRelPath = filepath.ToSlash(fullRelPath)
		
		if info.IsDir() {
			// Send subdirectory
			subDirInfo := FileInfo{
				Filename:    fullRelPath,
				FileSize:    0,
				IsDirectory: true,
			}
			
			if err := sendFileInfo(conn, subDirInfo); err != nil {
				return fmt.Errorf("failed to send subdirectory info: %w", err)
			}
			fmt.Printf("  Sending subdirectory: %s\n", fullRelPath)
		} else {
			// Send file with full relative path
			if err := sendSingleFile(conn, path, fullRelPath); err != nil {
				return fmt.Errorf("failed to send file %s: %w", path, err)
			}
		}
		
		return nil
	})
}

// sendFileRaw sends a file or directory using raw TCP connection
func sendFileRaw(conn net.Conn, filePath string) error {
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return err
	}
	
	if fileInfo.IsDir() {
		// Handle directory recursively
		return sendDirectoryRaw(conn, filePath)
	} else {
		// Handle single file - send with just the filename
		return sendSingleFile(conn, filePath, filepath.Base(filePath))
	}
}

// receiveFileRaw receives a file using raw TCP connection
func receiveFileRaw(conn net.Conn) error {
	// Keep receiving files until connection is closed
	for {
		// Receive file metadata
		fi, err := receiveFileInfo(conn)
		if err != nil {
			// Check if it's EOF (connection closed)
			if err == io.EOF || strings.Contains(err.Error(), "closed") || 
			   strings.Contains(err.Error(), "reset") || 
			   strings.Contains(err.Error(), "unexpected EOF") {
				return nil // Normal connection termination
			}
			return fmt.Errorf("failed to receive file info: %w", err)
		}
		
		// Ensure receive directory exists
		if err := os.MkdirAll(receiveDir, 0755); err != nil {
			return fmt.Errorf("cannot create directory: %w", err)
		}
		
		// Create full path - use the received filename which may include subdirectories
		// Normalize path separators
		normalizedPath := filepath.FromSlash(fi.Filename)
		fullPath := filepath.Join(receiveDir, normalizedPath)
		
		if fi.IsDirectory {
			// Create directory (including parent directories if needed)
			if err := os.MkdirAll(fullPath, 0755); err != nil {
				return fmt.Errorf("cannot create directory: %w", err)
			}
			fmt.Printf("Directory created: %s\n", fullPath)
			continue
		}
		
		// For files, ensure parent directory exists
		parentDir := filepath.Dir(fullPath)
		if parentDir != "." && parentDir != receiveDir {
			if err := os.MkdirAll(parentDir, 0755); err != nil {
				return fmt.Errorf("cannot create parent directory: %w", err)
			}
		}
		
		fmt.Printf("Receiving %s (%s)...\n", fi.Filename, formatBytes(float64(fi.FileSize)))
		
		// Create file with original filename
		file, err := os.Create(fullPath)
		if err != nil {
			return fmt.Errorf("cannot create file: %w", err)
		}
		
		// Create progress tracker
		progressWriter = NewProgressWriter(fi.FileSize, filepath.Base(fi.Filename))
		
		// Receive file data in chunks
		buffer := make([]byte, chunkSize)
		totalReceived := int64(0)
	
		for totalReceived < fi.FileSize {
			// Calculate remaining bytes
			remaining := fi.FileSize - totalReceived
			readSize := chunkSize
			if remaining < int64(chunkSize) {
				readSize = int(remaining)
			}
			
			// Read chunk from connection
			n, err := io.ReadFull(conn, buffer[:readSize])
			if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
				file.Close()
				return err
			}
			
			// Write chunk to file
			if _, err := file.Write(buffer[:n]); err != nil {
				file.Close()
				return err
			}
			
			totalReceived += int64(n)
			progressWriter.current = totalReceived
			progressWriter.printProgress()
			
			if totalReceived >= fi.FileSize {
				break
			}
		}
		
		// Close file and finalize progress
		file.Close()
		
		// Force final progress update to show 100%
		progressWriter.current = fi.FileSize
		progressWriter.printProgress()
		progressWriter.Finalize()
		
		fmt.Printf("\nFile received: %s\n\n", fullPath)
	}
}

// startServerRaw starts a raw TCP server for receiving files
func startServerRaw() {
	fmt.Printf("Files will be saved to: %s/\n", receiveDir)
	fmt.Println("Listening...")
	
	// Ensure receive directory exists
	if err := os.MkdirAll(receiveDir, 0755); err != nil {
		log.Fatalf("Cannot create directory: %v", err)
	}
	
	// Start TCP listener
	listener, err := net.Listen("tcp", port)
	if err != nil {
		log.Fatalf("Cannot start server: %v", err)
	}
	defer listener.Close()
	
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Connection error: %v", err)
			continue
		}
		
		go func(c net.Conn) {
			defer c.Close()
			if err := receiveFileRaw(c); err != nil {
				log.Printf("Error receiving file: %v", err)
			}
		}(conn)
	}
}

// sendFilesRaw sends files using raw TCP connection via Tor
func sendFilesRaw(target string, filePatterns []string) error {
	startTime = time.Now()
	
	// Clean target address (remove port if present)
	target = strings.Split(target, ":")[0]
	fullTarget := target + port
		
	// Create Tor dialer
	dialer, err := proxy.SOCKS5("tcp", torProxyAddr, nil, proxy.Direct)
	if err != nil {
		return fmt.Errorf("can't connect to Tor proxy: %v", err)
	}
	
	// Establish connection via Tor
	conn, err := dialer.Dial("tcp", fullTarget)
	if err != nil {
		return fmt.Errorf("failed to connect to target: %w", err)
	}
	defer conn.Close()
	
	// Collect all items to send
	var itemsToSend []string
	for _, pattern := range filePatterns {
		// Check if pattern contains wildcards
		if strings.Contains(pattern, "*") || strings.Contains(pattern, "?") || 
		   strings.Contains(pattern, "[") {
			// Use glob for patterns
			matches, err := filepath.Glob(pattern)
			if err != nil {
				return fmt.Errorf("invalid pattern %s: %w", pattern, err)
			}
			itemsToSend = append(itemsToSend, matches...)
		} else {
			// No wildcards, treat as literal path
			itemsToSend = append(itemsToSend, pattern)
		}
	}
	
	// Remove duplicates
	seen := make(map[string]bool)
	var uniqueItems []string
	for _, item := range itemsToSend {
		if !seen[item] {
			seen[item] = true
			uniqueItems = append(uniqueItems, item)
		}
	}
	
	if len(uniqueItems) == 0 {
		return fmt.Errorf("no files or directories found matching patterns: %v", filePatterns)
	}
	
	fmt.Printf("Found %d item(s) to send\n", len(uniqueItems))
	
	// Send each item
	for i, itemPath := range uniqueItems {
		fmt.Printf("[%d/%d] ", i+1, len(uniqueItems))
		
		if err := sendFileRaw(conn, itemPath); err != nil {
			return fmt.Errorf("failed to send %s: %w", itemPath, err)
		}
		
		if i < len(uniqueItems)-1 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	
	conn.Close()
	
	totalTime := time.Since(startTime)
	fmt.Printf("\nAll items transferred successfully in %s\n", formatDuration(totalTime))
	
	return nil
}

// sendStdinRaw sends data from stdin as a file
func sendStdinRaw(target string) error {
	startTime = time.Now()
	
	// Clean target address
	target = strings.Split(target, ":")[0]
	fullTarget := target + port
		
	// Create Tor dialer
	dialer, err := proxy.SOCKS5("tcp", torProxyAddr, nil, proxy.Direct)
	if err != nil {
		return fmt.Errorf("can't connect to Tor proxy: %v", err)
	}
	
	// Establish connection via Tor
	conn, err := dialer.Dial("tcp", fullTarget)
	if err != nil {
		return fmt.Errorf("failed to connect to target: %w", err)
	}
	defer conn.Close()
	
	// Read all data from stdin
	fmt.Println("Reading from stdin...")
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("error reading from stdin: %w", err)
	}
	
	if len(data) == 0 {
		return fmt.Errorf("no data received from stdin")
	}
	
	// For stdin data, we use a default filename
	fi := FileInfo{
		Filename:    "data.bin",
		FileSize:    int64(len(data)),
		IsDirectory: false,
	}
	
	// Send file metadata
	if err := sendFileInfo(conn, fi); err != nil {
		return fmt.Errorf("failed to send file info: %w", err)
	}
	
	// Create progress tracker
	progressWriter = NewProgressWriter(fi.FileSize, fi.Filename)
	defer progressWriter.Finalize()
	
	fmt.Printf("Sending %s (%s)...\n", fi.Filename, formatBytes(float64(fi.FileSize)))
	
	// Send data in chunks
	totalSent := int64(0)
	reader := bytes.NewReader(data)
	buffer := make([]byte, chunkSize)
	
	for {
		n, err := reader.Read(buffer)
		if err != nil && err != io.EOF {
			return err
		}
		
		if n == 0 {
			break
		}
		
		// Write chunk to connection
		if _, err := conn.Write(buffer[:n]); err != nil {
			return err
		}
		
		// Update progress
		totalSent += int64(n)
		progressWriter.current = totalSent
		progressWriter.printProgress()
		
		if err == io.EOF {
			break
		}
	}
	
	// Force final progress update to show 100%
	progressWriter.current = fi.FileSize
	progressWriter.printProgress()
	
	return nil
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "OnionTransfer - File transfer over Tor\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  Receiver mode (listen for files):\n")
		fmt.Fprintf(os.Stderr, "    %s\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  Sender mode (send files):\n")
		fmt.Fprintf(os.Stderr, "    %s <onion-address> file1.txt file2.jpg *.png directory/\n", os.Args[0])
	}
	
	flag.Parse()
	
	// If arguments exist, use sender mode
	if len(flag.Args()) > 0 {
		target := flag.Args()[0]
		
		// Check if data is coming from stdin
		stat, _ := os.Stdin.Stat()
		if (stat.Mode() & os.ModeCharDevice) == 0 {
			// Stdin has data
			err := sendStdinRaw(target)
			if err != nil {
				fmt.Printf("Error sending from stdin: %v\n", err)
				os.Exit(1)
			}
		} else if len(flag.Args()) > 1 {
			// Send files matching patterns
			filePatterns := flag.Args()[1:]
			err := sendFilesRaw(target, filePatterns)
			if err != nil {
				fmt.Printf("Error sending files: %v\n", err)
				os.Exit(1)
			}
		} else {
			// Show usage if only onion address is provided without files
			fmt.Println("Error: Please specify files or directories to send")
			fmt.Println()
			flag.Usage()
			os.Exit(1)
		}
	} else {
		// Otherwise use listener mode
		startServerRaw()
	}
}
