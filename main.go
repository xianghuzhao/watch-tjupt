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
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
	"gopkg.in/toast.v1"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

type config struct {
	DBFile     string   `json:"db_file"`
	Cookie     string   `json:"cookie"`
	UserAgent  string   `json:"user_agent"`
	Interval   int      `json:"interval"`
	Delay      int      `json:"delay"`
	SaveDir    string   `json:"save_dir"`
	IgnoreType []string `json:"ignore_type"`
	Skip       [][]int  `json:"skip"`
}

var cfg config

type torrent struct {
	gorm.Model
	TorrentID int `gorm:"index"`
	Title     string
	URL       string
	Page      string
	Type      string `gorm:"index"`
	Size      string
	Time      time.Time `gorm:"index"`
	Promotion string
	Sticky    int
}

var db *gorm.DB

var torrentsURL = "https://tjupt.org/torrents.php"
var hostURL = "https://tjupt.org/"

var configFilename = "config.json"
var logFilename = "watch-tjupt.log"

var logger *log.Logger

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

	var torrents []torrent

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

		var torrentPromotion string
		switch {
		case torrentLine.Eq(1).Find(".free").Length() > 0:
			torrentPromotion = "free"
		case torrentLine.Eq(1).Find(".twoupfree").Length() > 0:
			torrentPromotion = "2Xfree"
		case torrentLine.Eq(1).Find(".thirtypercent").Length() > 0:
			torrentPromotion = "30%"
		case torrentLine.Eq(1).Find(".twouphalfdown").Length() > 0:
			torrentPromotion = "2X50%"
		case torrentLine.Eq(1).Find(".halfdown").Length() > 0:
			torrentPromotion = "50%"
		case torrentLine.Eq(1).Find(".twoup").Length() > 0:
			torrentPromotion = "2X"
		default:
			torrentPromotion = "none"
		}

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

		if torrentPromotion == "none" && torrentSticky == 0 {
			return
		}

		imgDownload := torrentLine.Eq(1).Find("img.download")
		torrentURL, exist := imgDownload.Parent().Parent().Find("a").Attr("href")
		if !exist {
			logger.Println("Not found the image href")
			return
		}
		torrentIDStr := torrentURL[16:]
		torrentID, err := strconv.Atoi(torrentIDStr)
		if err != nil {
			logger.Printf("Invalid torrent ID: %s\n", torrentIDStr)
			return
		}
		torrentURL = hostURL + torrentURL

		titleText := torrentLine.Eq(1).Find("a b")
		torrentTitle, exist := titleText.Parent().Parent().Find("a").Attr("title")
		if !exist {
			logger.Println("Not found the torrent title")
			return
		}
		torrentPage := hostURL + "details.php?id=" + torrentIDStr

		t := &torrent{}
		exist = !db.Where("torrent_id = ?", torrentID).First(t).RecordNotFound()
		if exist {
			//logger.Printf("Torrent already there: %s\n", torrentTitle)
			return
		}

		typeImg := torrentLine.Eq(0).Find("img")
		torrentType := typeImg.AttrOr("title", "Unknown")
		for _, tp := range cfg.IgnoreType {
			if tp == torrentType {
				return
			}
		}

		torrentSize := torrentLine.Eq(4).Text()

		t = &torrent{
			TorrentID: torrentID,
			Title:     torrentTitle,
			URL:       torrentURL,
			Page:      torrentPage,
			Type:      torrentType,
			Size:      torrentSize,
			Time:      torrentTime,
			Promotion: torrentPromotion,
			Sticky:    torrentSticky,
		}

		logger.Println("Found torrent: ", torrentSticky, torrentID, torrentTime, torrentPromotion, torrentType, torrentSize, torrentTitle)

		download(torrentURL)

		torrents = append(torrents, *t)
	})

	return torrents
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
			Title:               fmt.Sprintf("%s %s %s %s %s", typeGBK, stickyGBK, t.Promotion, t.Size, t.Time.Format("15:04:05")),
			Message:             titleGBK,
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

func sortTorrents(torrents []torrent) {
	sort.Slice(torrents, func(i, j int) bool {
		return torrents[i].Time.Before(torrents[j].Time)
	})
}

func saveTorrents(torrents []torrent) {
	for _, t := range torrents {
		db.Create(&t)
	}
}

func search() {
	logger.Println("Searching for new torrent...")

	torrents := getPage()

	sortTorrents(torrents)

	saveTorrents(torrents)

	logger.Println("Searching finished")

	notify(torrents)
}

func timer() {
	skipper := 0
	nowHour := time.Now().Hour()

	for _, skipLine := range cfg.Skip {
		if nowHour >= skipLine[0] && nowHour < skipLine[1] {
			skipper = skipLine[2]
		}
	}

	if skipper == 0 || rand.Intn(skipper) != 0 {
		return
	}

	time.Sleep(time.Duration(rand.Intn(cfg.Delay)) * time.Second)
	search()
}

func run() {
	ticker := time.NewTicker(time.Duration(cfg.Interval) * time.Second)
	stop := make(chan struct{})
	wg := sync.WaitGroup{}
	wg.Add(1)

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

func loadDB() {
	var err error
	db, err = gorm.Open("sqlite3", cfg.DBFile)
	if err != nil {
		logger.Panicf("Failed to connect to database: %s\n", cfg.DBFile)
	}

	db.AutoMigrate(&torrent{})
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

	loadDB()
	defer db.Close()

	run()

	logger.Println("Exit application")
	logger.Println("--------------------------------------------------------------------------------")
}
