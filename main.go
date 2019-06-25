package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
	"gopkg.in/toast.v1"
)

type config struct {
	Cookie    string `json:"cookie"`
	UserAgent string `json:"user_agent"`
	MinDiff   int    `json:"min_diff"`
	Interval  int    `json:"interval"`
	Delay     int    `json:"delay"`
	SaveDir   string `json:"save_dir"`
}

var cfg config

type torrent struct {
	Title  string
	URL    string
	Page   string
	Type   string
	Size   string
	Time   time.Time
	Free   bool
	Sticky int
}

var torrentsURL = "https://tjupt.org/torrents.php"
var hostURL = "https://tjupt.org/"

var configFilename = "config.json"
var logFilename = "watch-tjupt.log"

var logger *log.Logger

var torrentPool = make(map[string]torrent)

var lastCheckTime time.Time

func encodeGBK(s string) (string, error) {
	I := bytes.NewReader([]byte(s))
	O := transform.NewReader(I, simplifiedchinese.GBK.NewEncoder())
	d, e := ioutil.ReadAll(O)
	if e != nil {
		return "", e
	}
	return string(d), nil
}

func download(url string) {
	client := &http.Client{}
	req, _ := http.NewRequest("GET", url, nil)

	req.Header.Add("Cookie", cfg.Cookie)
	req.Header.Add("User-Agent", cfg.UserAgent)
	req.Header.Add("Referer", torrentsURL)

	res, err := client.Do(req)
	if err != nil {
		logger.Println("Download page error:", err)
		return
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		logger.Printf("Get error %d: %s\n", res.StatusCode, res.Status)
		return
	}

	contentDisposition := res.Header.Get("content-disposition")
	_, params, err := mime.ParseMediaType(contentDisposition)
	if err != nil {
		logger.Println("Parse contentDisposition error:", err)
	}
	filename := params["filename"]

	fullPath := path.Join(cfg.SaveDir, filename)
	out, err := os.Create(fullPath)
	if err != nil {
		return
	}
	defer out.Close()

	// Write the body to file
	_, err = io.Copy(out, res.Body)
	if err != nil {
		logger.Printf("Write to file error \"%s\": %s\n", fullPath, err)
	}
}

func getPage() []torrent {
	startGetPageTime := time.Now()

	client := &http.Client{}
	req, _ := http.NewRequest("GET", torrentsURL, nil)

	req.Header.Add("Cookie", cfg.Cookie)
	req.Header.Add("User-Agent", cfg.UserAgent)

	res, err := client.Do(req)
	if err != nil {
		logger.Println("Download page error:", err)
		return nil
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		logger.Printf("Get error %d: %s\n", res.StatusCode, res.Status)
		return nil
	}

	doc, err := goquery.NewDocumentFromReader(res.Body)
	if err != nil {
		logger.Println(err)
		return nil
	}

	var result []torrent

	doc.Find(".torrents tr").Each(func(i int, s *goquery.Selection) {
		torrentLine := s.Find("td.rowfollow")

		timeSpan := torrentLine.Eq(3).Find("span")
		torrentTimeStr, exist := timeSpan.Attr("title")
		if !exist {
			return
		}

		layout := "2006-01-02 15:04:05"
		torrentTime, err := time.ParseInLocation(layout, torrentTimeStr, time.Local)
		if err != nil {
			logger.Println("Parse time error:", err)
			return
		}
		if torrentTime.Before(lastCheckTime) {
			return
		}

		torrentFree := (torrentLine.Eq(1).Find(".free").Length() > 0)
		if !torrentFree {
			return
		}

		imgDownload := torrentLine.Eq(1).Find("img.download")
		torrentURL, exist := imgDownload.Parent().Parent().Find("a").Attr("href")
		if !exist {
			logger.Println("Not found the image href")
			return
		}
		torrentID := torrentURL[16:]
		torrentURL = hostURL + torrentURL

		titleText := torrentLine.Eq(1).Find("a b")
		torrentTitle, exist := titleText.Parent().Parent().Find("a").Attr("title")
		if !exist {
			logger.Println("Not found the torrent title")
			return
		}
		torrentPage := hostURL + "details.php?id=" + torrentID

		var torrentSticky int
		switch {
		case torrentLine.Eq(1).Find("img.sticky_1").Length() > 0:
			torrentSticky = 1
		case torrentLine.Eq(1).Find("img.sticky_2").Length() > 0:
			torrentSticky = 2
		case torrentLine.Eq(1).Find("img.sticky_3").Length() > 0:
			torrentSticky = 3
		default:
			torrentSticky = 0
		}

		_, exist = torrentPool[torrentID]
		if exist {
			logger.Printf("Torrent already there: %s\n", torrentTitle)
			return
		}

		typeImg := torrentLine.Eq(0).Find("img")
		torrentType := typeImg.AttrOr("title", "Unknown")

		torrentSize := torrentLine.Eq(4).Text()

		torrentPool[torrentID] = torrent{
			Title:  torrentTitle,
			URL:    torrentURL,
			Page:   torrentPage,
			Type:   torrentType,
			Size:   torrentSize,
			Time:   torrentTime,
			Free:   torrentFree,
			Sticky: torrentSticky,
		}

		logger.Println("Found torrent: ", torrentSticky, torrentID, torrentTime, torrentFree, torrentType, torrentSize, torrentTitle)

		download(torrentURL)

		result = append(result, torrentPool[torrentID])
	})

	lastCheckTime = startGetPageTime

	return result
}

func notify(torrents []torrent) {
	for _, t := range torrents {
		typeGBK, _ := encodeGBK(t.Type)
		titleGBK, _ := encodeGBK(t.Title)

		stickyGBK, _ := encodeGBK("普通")
		if t.Sticky != 0 {
			stickyGBK, _ = encodeGBK("置顶" + strconv.Itoa(t.Sticky))
		}

		notification := toast.Notification{
			AppID:               "Watch TJUPT",
			Title:               "Torrent Found",
			Message:             fmt.Sprintf("%s %s %s %s\n%s", stickyGBK, typeGBK, t.Size, t.Time.Format("15:04:05"), titleGBK),
			ActivationArguments: t.Page,
			Actions: []toast.Action{
				{Type: "protocol", Label: "Torrent list", Arguments: torrentsURL},
				{Type: "protocol", Label: "Download torrent", Arguments: t.URL},
			},
		}
		err := notification.Push()
		if err != nil {
			logger.Fatalln(err)
		}
	}
}

func cleanTorrent() {
	for k, v := range torrentPool {
		diff := time.Since(v.Time)
		if diff > time.Duration(cfg.MinDiff)*time.Second {
			logger.Println("Clean torrent:", v.Title)
			delete(torrentPool, k)
		}
	}
}

func search() {
	logger.Println("Searching for new torrent...")
	//cleanTorrent()
	notify(getPage())
}

func timer() {
	var skipper int
	nowHour := time.Now().Hour()
	switch {
	case nowHour < 2:
		skipper = 2
	case nowHour < 4:
		skipper = 5
	case nowHour < 8:
		skipper = 10
	case nowHour < 10:
		skipper = 2
	default:
		skipper = 1
	}

	if rand.Intn(skipper) == 0 {
		time.Sleep(time.Duration(rand.Intn(cfg.Delay)) * time.Second)
		search()
	}
}

func run() {
	ticker := time.NewTicker(time.Duration(cfg.Interval) * time.Second)
	stop := make(chan struct{})
	wg := sync.WaitGroup{}
	wg.Add(1)

	lastCheckTime = time.Now().Add(time.Duration(-cfg.MinDiff) * time.Second)

	search()

	go func() {
		defer wg.Done()
		for {
			select {
			case <-ticker.C:
				timer()
				//ticker.Stop()
				//return
			case <-stop:
				logger.Println("Stop the ticker")
				ticker.Stop()
				return
			}
		}
	}()

	quit := make(chan os.Signal)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	close(stop)
	wg.Wait()
}

func loadConfig(dir string) {
	buffer, err := ioutil.ReadFile(path.Join(dir, configFilename))
	if err != nil {
		logger.Panicf("Config file \"%s\" read error: %s\n", configFilename, err)
	}

	err = json.Unmarshal(buffer, &cfg)
	if err != nil {
		logger.Panic(err)
	}
}

func main() {
	dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		log.Fatal(err)
	}

	f, err := os.OpenFile(path.Join(dir, logFilename),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Println(err)
	}
	defer f.Close()

	logger = log.New(f, "[Watch TJUPT] ", log.LstdFlags)

	logger.Println("================================================================================")
	logger.Println("Start application")

	loadConfig(dir)
	run()

	logger.Println("Exit application")
	logger.Println("--------------------------------------------------------------------------------")
}
