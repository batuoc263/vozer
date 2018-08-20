package vozer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/sirupsen/logrus"
)

var (
	urlsMap       = sync.Map{}
	imagesMap     = sync.Map{}
	isCtxCanceled int32
)

var (
	mux         = &sync.RWMutex{}
	crawlFailed []int
)

type (
	CrawledPageMeta struct {
		PageNumber int
		Document   *goquery.Document
	}

	PageURLMeta struct {
		URL        string
		PageNumber int
		Retries    uint
	}

	URLMeta struct {
		URL     string `json:"url"`
		Text    string `json:"text"`
		Seen    int    `json:"seen"`
		AtPosts []int  `json:"at_posts"`
	}

	ImageMeta struct {
		URL      string `json:"url"`
		Filename string `json:"filename"`
		Seen     int    `json:"seen"`
		AtPosts  []int  `json:"at_posts"`
	}
)

func Crawl(ctx context.Context, cfg VozerConfig) error {
	logrus.Infof("start crawling thread")
	ensureDir(cfg.DestPath)

	firstPage, lastPageNu, err := getLastPageNu(cfg.ThreadURL)
	if err != nil {
		return err
	}

	crawledPageChan := make(chan *CrawledPageMeta, lastPageNu)
	crawledPageChan <- firstPage

	pageURLChan := make(chan *PageURLMeta, lastPageNu-1)

	go func(ctx context.Context) {
		pageWg := &sync.WaitGroup{}
		for i := uint(0); i < cfg.NuWorkers; i++ {
			pageWg.Add(1)
			go crawlPage(ctx, i, cfg, pageWg, pageURLChan, crawledPageChan)
		}
		pageWg.Wait()
		close(crawledPageChan)
		logrus.Infof("all pages crawled. Extracting data from pages...")
	}(ctx)

	for i := 2; i <= lastPageNu; i++ {
		pageURLChan <- &PageURLMeta{
			URL:        fmt.Sprintf("%s&page=%d", cfg.ThreadURL, i),
			PageNumber: i,
			Retries:    0,
		}
	}
	close(pageURLChan)

	imageChan := make(chan *ImageMeta)
	imageWg := &sync.WaitGroup{}
	if cfg.IsCrawlImages {
		imageChan = make(chan *ImageMeta, 5000)

		ensureDir(path.Join(cfg.DestPath, "img"))
		for i := uint(0); i < cfg.NuWorkers; i++ {
			imageWg.Add(1)
			go crawlImage(ctx, i, imageWg, imageChan, cfg.DestPath)
		}
	}
	crawlData(ctx, cfg, crawledPageChan, imageChan) // Possible to use multiple workers here if really necessary
	close(imageChan)
	imageWg.Wait()

	if atomic.LoadInt32(&isCtxCanceled) == 0 {
		logrus.Infof("done extracting data")
		exportMetadataToFiles(cfg)
	}
	return nil
}

func getLastPageNu(url string) (*CrawledPageMeta, int, error) {
	resp, err := http.Get(url)
	if err != nil || resp.StatusCode/200 != 1 {
		return nil, -1, errors.New("failed to crawl first page from thread")
	}
	firstPage, err := goquery.NewDocumentFromReader(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, -1, err
	}
	pageControlStr := firstPage.Find("div.neo_column.main table").First().Find("td.vbmenu_control").Text() // Page 1 of 100
	if pageControlStr == "" { // Thread with only 1 page
		return &CrawledPageMeta{1, firstPage}, 1, nil
	}
	lastPageStr := pageControlStr[strings.LastIndex(pageControlStr, " ")+1:]
	lastPageNu, err := strconv.Atoi(lastPageStr)
	if err != nil {
		return nil, -1, err
	}
	return &CrawledPageMeta{1, firstPage}, lastPageNu, nil
}

func crawlPage(ctx context.Context, idx uint, cfg VozerConfig, wg *sync.WaitGroup, pageURLChan chan *PageURLMeta, crawledPageChan chan<- *CrawledPageMeta) {
	defer wg.Done()

	client := &http.Client{}

	for {
		select {
		case <-ctx.Done():
			atomic.StoreInt32(&isCtxCanceled, 1)
			logrus.Infof("page crawler #%d: Terminated", idx)
			return
		case meta, ok := <-pageURLChan:
			if !ok {
				logrus.Infof("page crawler #%d: Done", idx)
				return
			}

			// TODO: Using channel to retry.
			// Currently have problem if using poison pill to notify channel closing.
			//if meta.Retries == cfg.Retries {
			//	logrus.Warnf("MAX_RETRIES: failed to crawl page #%d (%s)", meta.PageNumber, meta.URL)
			//	continue
			//}
			//time.Sleep(200*time.Millisecond)

			retryCrawlingPage(cfg, idx, meta, client, crawledPageChan)
		}
	}
}

func retryCrawlingPage(cfg VozerConfig, idx uint, meta *PageURLMeta, client *http.Client, crawledPageChan chan<- *CrawledPageMeta) {
	for i := uint(1); i <= cfg.Retries; i++ {
		logrus.Debugf("page crawler #%d: crawling page %d (%s)", idx, meta.PageNumber, meta.URL)
		resp, err := client.Get(meta.URL)
		if err != nil || resp.StatusCode/200 != 1 {
			time.Sleep(time.Duration(rand.Intn(8)+2) * time.Second)
			continue
		}

		doc, err := goquery.NewDocumentFromReader(resp.Body)
		resp.Body.Close()
		if err != nil {
			time.Sleep(time.Duration(rand.Intn(8)+2) * time.Second)
			continue
		}
		crawledPageChan <- &CrawledPageMeta{meta.PageNumber, doc}
		logrus.Infof("page crawler #%d: successfully crawled page #%d (%s)", idx, meta.PageNumber, meta.URL)
		return
	}

	mux.Lock()
	crawlFailed = append(crawlFailed, meta.PageNumber)
	mux.Unlock()
	logrus.Errorf("MAX_RETRY: failed to crawl page #%d (%s)", meta.PageNumber, meta.URL)
}

func crawlData(ctx context.Context, cfg VozerConfig, crawledPageChan <-chan *CrawledPageMeta, imageChan chan<- *ImageMeta) {
	for {
		select {
		case <-ctx.Done():
			atomic.StoreInt32(&isCtxCanceled, 1)
			logrus.Infof("extract data: Terminated")
			return
		case page, ok := <-crawledPageChan:
			if !ok {
				logrus.Infof("extract data: Done")
				return
			}

			page.Document.Find("table.tborder.voz-postbit").Each(func(i int, s *goquery.Selection) {
				postCountStr, _ := s.Find("tbody tr").First().Find("td div").First().Find("a").First().Attr("name")
				postCount, _ := strconv.Atoi(postCountStr)

				s.Find("div.voz-post-message").Each(func(i int, s *goquery.Selection) {
					if cfg.IsCrawlURLs {
						extractURLs(s, postCount)
					}
					if cfg.IsCrawlImages {
						extractImageURLs(s, postCount, imageChan)
					}
				})
			})
		}
	}
}

func extractURLs(post *goquery.Selection, postCount int) {
	post.Find("a").Each(func(i int, s *goquery.Selection) {
		href, ok := s.Attr("href")
		if ok && href != "" {
			href = normalizeURL(href)
			v, existed := urlsMap.Load(href)
			if existed {
				meta := v.(URLMeta)
				meta.Seen++
				meta.AtPosts = append(meta.AtPosts, postCount)
				urlsMap.Store(href, meta)
				return
			}

			urlsMap.Store(href, URLMeta{
				URL:     href,
				Text:    s.Text(),
				Seen:    1,
				AtPosts: []int{postCount},
			})
		}
	})
}

func extractImageURLs(post *goquery.Selection, postCount int, imageChan chan<- *ImageMeta) {
	post.Find("img").Each(func(idx int, s *goquery.Selection) {
		imgURL, ok := s.Attr("src")
		if !ok {
			return
		}
		if strings.HasPrefix(imgURL, "https://") || strings.HasPrefix(imgURL, "http://") {
			v, existed := imagesMap.Load(imgURL)
			if existed {
				meta := v.(ImageMeta)
				meta.Seen++
				meta.AtPosts = append(meta.AtPosts, postCount)
				imagesMap.Store(imgURL, meta)
				return
			}

			meta := ImageMeta{
				URL:      imgURL,
				Filename: imgURL[strings.LastIndex(imgURL, "/")+1:],
				Seen:     1,
				AtPosts:  []int{postCount},
			}
			imagesMap.Store(imgURL, meta)
			imageChan <- &meta
		}
	})
}

func crawlImage(ctx context.Context, idx uint, wg *sync.WaitGroup, imageChan <-chan *ImageMeta, destPath string) {
	defer wg.Done()

	client := http.Client{}

	for {
		select {
		case <-ctx.Done():
			atomic.StoreInt32(&isCtxCanceled, 1)
			logrus.Infof("image crawler #%d: Terminated", idx)
			return
		case meta, ok := <-imageChan:
			if !ok {
				logrus.Infof("image crawler #%d: Done", idx)
				return
			}

			resp, err := client.Get(meta.URL)
			if err != nil {
				logrus.Errorf("image crawler #%d: failed to crawl image \"%s\": %s", idx, meta.URL, err)
				continue
			}
			if resp.StatusCode/200 != 1 {
				logrus.Errorf("image crawler #%d: failed to crawl image \"%s\": %s", idx, meta.URL, resp.Status)
				continue
			}
			b, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				resp.Body.Close()
				continue
			}
			resp.Body.Close()
			fp := path.Join(destPath, "img", meta.Filename)
			if err := ioutil.WriteFile(fp, b, 0644); err != nil {
				logrus.Errorf("image crawler #%d: failed to write image to %s: %s", idx, fp, err)
				continue
			}
			logrus.Infof("image crawler #%d: %s -> %s", idx, meta.URL, meta.Filename)
		}
	}
}

func normalizeURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	if u.Host == "" {
		if u.Path == "/redirect/index.php" {
			ueu, _ := url.QueryUnescape(u.Query().Get("link"))
			return ueu
		}
		return "https://forums.voz.vn/" + strings.TrimPrefix(rawURL, "/")
	}
	return rawURL
}

func ensureDir(p string) {
	if _, err := os.Stat(p); os.IsNotExist(err) {
		os.MkdirAll(p, 0777) // TODO: Permission
	}
}

func exportMetadataToFiles(cfg VozerConfig) {
	if cfg.IsCrawlURLs {
		var urls []URLMeta
		urlsMap.Range(func(k, v interface{}) bool {
			urls = append(urls, v.(URLMeta))
			return true
		})
		sort.Sort(bySeenURL(urls))
		writeToFile(path.Join(cfg.DestPath, "urls_meta.json"), urls)
	}

	if cfg.IsCrawlImages {
		var images []ImageMeta
		imagesMap.Range(func(k, v interface{}) bool {
			images = append(images, v.(ImageMeta))
			return true
		})
		sort.Sort(bySeenImage(images))
		writeToFile(path.Join(cfg.DestPath, "images_meta.json"), images)
	}

	mux.RLock()
	if len(crawlFailed) > 0 {
		writeToFile(path.Join(cfg.DestPath, "failed_pages.json"), crawlFailed)
	}
	mux.RUnlock()
}

func writeToFile(fp string, data interface{}) {
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		logrus.Errorf("failed to export images metadata: %s", err)
		return
	}
	if err := ioutil.WriteFile(fp, b, 0644); err != nil {
		logrus.Errorf("failed to write metadata to file: %s", err)
		return
	}
	logrus.Infof("metadata has been exported to %s", fp)
}

type (
	bySeenURL []URLMeta
	bySeenImage []ImageMeta
)

func (u bySeenURL) Len() int           { return len(u) }
func (u bySeenURL) Swap(i, j int)      { u[i], u[j] = u[j], u[i] }
func (u bySeenURL) Less(i, j int) bool { return u[i].Seen < u[j].Seen }

func (img bySeenImage) Len() int           { return len(img) }
func (img bySeenImage) Swap(i, j int)      { img[i], img[j] = img[j], img[i] }
func (img bySeenImage) Less(i, j int) bool { return img[i].Seen < img[j].Seen }
