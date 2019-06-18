package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
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
}

var cfg config

type torrent struct {
	Title string
	URL   string
	Type  string
	Size  string
	Time  time.Time
	Free  bool
}

var torrentURL = "https://tjupt.org/torrents.php"
var hostURL = "https://tjupt.org/"

var configFilename = "config.json"
var logFilename = "watch-tjupt.log"

var logger *log.Logger

var torrentPool = make(map[string]torrent)

func encodeGBK(s string) (string, error) {
	I := bytes.NewReader([]byte(s))
	O := transform.NewReader(I, simplifiedchinese.GBK.NewEncoder())
	d, e := ioutil.ReadAll(O)
	if e != nil {
		return "", e
	}
	return string(d), nil
}

func getPage() []torrent {
	client := &http.Client{}
	req, _ := http.NewRequest("GET", torrentURL, nil)

	req.Header.Add("Cookie", cfg.Cookie)
	req.Header.Add("User-Agent", cfg.UserAgent)

	res, err := client.Do(req)
	if err != nil {
		logger.Fatalln("Download page error:", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		logger.Fatalf("Get error %d: %s\n", res.StatusCode, res.Status)
	}

	doc, err := goquery.NewDocumentFromReader(res.Body)
	if err != nil {
		logger.Fatalln(err)
	}

	var result []torrent

	doc.Find(".torrents tr").Each(func(i int, s *goquery.Selection) {
		torrentLine := s.Find("td.rowfollow")

		timeSpan := torrentLine.Eq(3).Find("span")
		torrentTimeStr, exist := timeSpan.Attr("title")
		if !exist {
			return
		}

		loc, _ := time.LoadLocation("Local")
		layout := "2006-01-02 15:04:05"
		torrentTime, err := time.ParseInLocation(layout, torrentTimeStr, loc)
		if err != nil {
			logger.Fatalln(err)
		}
		diff := time.Now().Sub(torrentTime)
		if diff > time.Duration(cfg.MinDiff)*time.Second {
			return
		}

		torrentFree := (torrentLine.Eq(1).Find(".free").Length() > 0)
		if !torrentFree {
			return
		}

		imgDownload := torrentLine.Eq(1).Find("img.download")
		torrentURL, exist := imgDownload.Parent().Parent().Find("a").Attr("href")
		if !exist {
			logger.Println("Not exist a href")
			return
		}
		torrentID := torrentURL[16:]

		torrentTitle := torrentLine.Eq(1).Find("a b").Text()

		_, exist = torrentPool[torrentID]
		if exist {
			logger.Printf("Torrent already there: %s\n", torrentTitle)
			return
		}

		torrentURL = hostURL + torrentURL

		typeImg := torrentLine.Eq(0).Find("img")
		torrentType := typeImg.AttrOr("title", "Unknown")

		torrentSize := torrentLine.Eq(4).Text()

		torrentPool[torrentID] = torrent{
			Title: torrentTitle,
			URL:   torrentURL,
			Type:  torrentType,
			Size:  torrentSize,
			Time:  torrentTime,
			Free:  torrentFree,
		}

		logger.Println(torrentID, torrentTime, torrentFree, torrentType, torrentSize, torrentTitle)

		result = append(result, torrentPool[torrentID])
	})

	return result
}

func notify(torrents []torrent) {
	for _, t := range torrents {
		typeGBK, _ := encodeGBK(t.Type)
		titleGBK, _ := encodeGBK(t.Title)

		notification := toast.Notification{
			AppID:   "Watch TJUPT",
			Title:   "New Post",
			Message: fmt.Sprintf("%s %s\n%s", typeGBK, t.Size, titleGBK),
			//Icon:    "go.png", // This file must exist (remove this line if it doesn't)
			Actions: []toast.Action{
				{Type: "protocol", Label: "Open webpage", Arguments: torrentURL},
				{Type: "protocol", Label: "Download torrent", Arguments: t.URL},
				{Type: "protocol", Label: "Dismiss", Arguments: ""},
			},
		}
		err := notification.Push()
		if err != nil {
			logger.Fatalln(err)
		}
	}
}

func find() {
	logger.Println("Start to find")
	notify(getPage())
}

func run() {
	ticker := time.NewTicker(time.Duration(cfg.Interval) * time.Second)
	stop := make(chan struct{})
	wg := sync.WaitGroup{}
	wg.Add(1)

	find()

	go func() {
		defer wg.Done()
		for {
			select {
			case <-ticker.C:
				time.Sleep(time.Duration(rand.Intn(cfg.Delay)) * time.Second)
				find()
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
