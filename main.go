package main

import (
	"compress/gzip"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

// Config 配置文件结构体，对应config.yaml的所有配置项
type Config struct {
	Server struct {
		ListenPort   int    `yaml:"listen_port"`
		CronSchedule string `yaml:"cron_schedule"`
		TimeZone     string `yaml:"timezone"`
	} `yaml:"server"`

	Cache struct {
		DownloadURL string `yaml:"epg_url"`      // 节目单下载地址
		DownloadDir string `yaml:"download_dir"` // 本地下载路径
		CacheFile   string `yaml:"file"`         // 本地缓存路径
	} `yaml:"cache"`

	Log struct {
		Level string `yaml:"level"` // 日志级别: debug/info/warn/error
	} `yaml:"log"`
}

const (
	configFile    = "config.yaml" // 命令行指定的配置文件路径
	LogLevelDebug = "debug"
	LogLevelInfo  = "info"
	LogLevelWarn  = "warn"
	LogLevelError = "error"
)

// 全局配置
var (
	config Config      // 全局配置对象
	logger *log.Logger // 全局日志对象
)

// XML结构定义（对应节目单XML格式）
type TV struct {
	XMLName           xml.Name    `xml:"tv"`
	Channels          []Channel   `xml:"channel"`
	Programmes        []Programme `xml:"programme"`
	GeneratorInfoName string      `xml:"generator-info-name,attr"`
	GeneratorInfoURL  string      `xml:"generator-info-url,attr"`
	SourceInfoName    string      `xml:"source-info-name,attr"`
	SourceInfoURL     string      `xml:"source-info-url,attr"`
}

type Channel struct {
	ID          string        `xml:"id,attr"`
	DisplayName []DisplayName `xml:"display-name"`
}

type DisplayName struct {
	Lang  string `xml:"lang,attr"`
	Value string `xml:",chardata"`
}

type Programme struct {
	Start   string `xml:"start,attr"`
	Stop    string `xml:"stop,attr"`
	Channel string `xml:"channel,attr"`
	Title   string `xml:"title"`
}

// 内存缓存结构
type EPGCache struct {
	ChannelMap  map[string]string                   // 频道名称→频道ID
	ProgramData map[string]map[string][]ProgramItem // 频道ID→日期→节目列表
	mu          sync.RWMutex
}

type ProgramItem struct {
	Start string `json:"start"`
	End   string `json:"end"`
	Title string `json:"title"`
}

// 接口返回结构
type EPGResponse struct {
	ChannelName string        `json:"channel_name"`
	Date        string        `json:"date"`
	EPGData     []ProgramItem `json:"epg_data"`
}

var (
	epgCache = &EPGCache{
		ChannelMap:  make(map[string]string),
		ProgramData: make(map[string]map[string][]ProgramItem),
	}
)

// 初始化日志
func initLogger() {
	logger = log.New(os.Stdout, "", log.LstdFlags)
}

// 封装日志输出函数
func logDebug(format string, v ...interface{}) {
	if config.Log.Level == LogLevelDebug {
		logger.Printf("[DEBUG] [%s] %s", time.Now().Format("2006-01-02 15:04:05.000"), fmt.Sprintf(format, v...))
	}
}

func logInfo(format string, v ...interface{}) {
	if config.Log.Level == LogLevelDebug || config.Log.Level == LogLevelInfo {
		logger.Printf("[INFO]  [%s] %s", time.Now().Format("2006-01-02 15:04:05.000"), fmt.Sprintf(format, v...))
	}
}

func logWarn(format string, v ...interface{}) {
	if config.Log.Level == LogLevelDebug || config.Log.Level == LogLevelInfo || config.Log.Level == LogLevelWarn {
		logger.Printf("[WARN]  [%s] %s", time.Now().Format("2006-01-02 15:04:05.000"), fmt.Sprintf(format, v...))
	}
}

func logError(format string, v ...interface{}) {
	logger.Printf("[ERROR] [%s] %s", time.Now().Format("2006-01-02 15:04:05.000"), fmt.Sprintf(format, v...))
}

// loadConfig 加载YAML配置文件
func loadConfig(filePath string) error {
	// 读取配置文件内容
	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("读取配置文件失败: %w", err)
	}

	// 解析YAML到config结构体
	if err := yaml.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("解析YAML配置失败: %w", err)
	}

	// 设置默认值（防止配置文件未配置时为空）
	setDefaultConfig()

	return nil
}

// setDefaultConfig 设置配置默认值
func setDefaultConfig() {
	// Server默认值
	if config.Server.ListenPort == 0 {
		config.Server.ListenPort = 8090
	}
	if config.Server.CronSchedule == "" {
		config.Server.CronSchedule = "0 0 * * *"
	}
	if config.Server.TimeZone == "" {
		config.Server.TimeZone = "Asia/Shanghai"
	}

	// Cache默认值
	if config.Cache.DownloadURL == "" {
		config.Cache.DownloadURL = "http://epg.51zmt.top:8000/e.xml.gz"
	}
	if config.Cache.DownloadDir == "" {
		config.Cache.DownloadDir = "./epg_download"
	}
	if config.Cache.CacheFile == "" {
		config.Cache.CacheFile = "./epg_cache.json"
	}

	// Log默认值
	if config.Log.Level == "" {
		config.Log.Level = LogLevelInfo
	}
}

func main() {
	// 初始化日志
	initLogger()

	// 3. 加载配置文件
	if err := loadConfig(configFile); err != nil {
		logError("配置文件加载失败: %s", err)
		os.Exit(1)
	}

	// 设置时区
	loc, err := time.LoadLocation(config.Server.TimeZone)
	if err != nil {
		logError("加载时区失败: %s", err)
		return
	}
	time.Local = loc

	// 初始化目录
	if err := initDirs(); err != nil {
		logError("初始化目录失败: %s", err)
		return
	}

	// 加载缓存（如果存在）
	if err := loadCache(); err != nil {
		logWarn("加载缓存失败: %v 将重新下载", err)
		// 首次运行立即执行一次下载
		if err := downloadAndParseEPG(); err != nil {
			logError("首次下载解析失败: %v", err)
		}
	}

	// 启动定时任务
	c := cron.New(cron.WithLocation(loc))
	_, err = c.AddFunc(config.Server.CronSchedule, func() {
		logInfo("开始执行每日EPG更新任务")
		if err := downloadAndParseEPG(); err != nil {
			logError("定时任务执行失败: %v", err)
		} else {
			logInfo("定时任务执行成功")
		}
	})
	if err != nil {
		logError("创建定时任务失败: %v", err)
		return
	}
	c.Start()
	defer c.Stop()

	// 注册HTTP处理函数
	http.HandleFunc("/", epgQueryHandler)

	// 启动HTTP服务
	logInfo("EPG服务已启动，监听端口: %d", config.Server.ListenPort)
	if err := http.ListenAndServe(fmt.Sprintf(":%d", config.Server.ListenPort), nil); err != nil {
		logError("HTTP服务启动失败: %v", err)
	}
}

// 初始化目录
func initDirs() error {
	if err := os.MkdirAll(config.Cache.DownloadDir, 0755); err != nil {
		return err
	}
	return nil
}

// 下载并解析EPG数据
func downloadAndParseEPG() error {
	// 1. 下载gz文件
	logInfo("开始下载EPG文件...")
	tmpFile, err := downloadFile(config.Cache.DownloadURL)
	if err != nil {
		return fmt.Errorf("下载失败: %w", err)
	}
	defer os.Remove(tmpFile) // 下载完成后删除临时文件

	// 2. 解压文件
	logInfo("开始解压EPG文件...")
	xmlFilePath, err := extractTarGz(tmpFile, config.Cache.DownloadDir)
	if err != nil {
		return fmt.Errorf("解压失败: %w", err)
	}

	// 3. 解析XML文件
	logInfo("开始解析XML文件...")
	tvData, err := parseEPGXML(xmlFilePath)
	if err != nil {
		return fmt.Errorf("解析XML失败: %w", err)
	}

	// 4. 构建缓存
	logInfo("开始构建EPG缓存...")
	if err := buildEPGCache(tvData); err != nil {
		return fmt.Errorf("构建缓存失败: %w", err)
	}

	// 5. 保存缓存到文件
	logInfo("保存EPG缓存到文件...")
	if err := saveCache(); err != nil {
		return fmt.Errorf("保存缓存失败: %w", err)
	}

	return nil
}

// 下载文件到临时路径
func downloadFile(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP请求失败，状态码: %d", resp.StatusCode)
	}

	// 创建临时文件
	tmpFile, err := os.CreateTemp(config.Cache.DownloadDir, "epg_*.gz")
	if err != nil {
		return "", err
	}
	defer tmpFile.Close()

	// 写入文件
	_, err = io.Copy(tmpFile, resp.Body)
	if err != nil {
		return "", err
	}

	return tmpFile.Name(), nil
}

// 解压tar.gz文件
func extractTarGz(gzPath, destDir string) (string, error) {
	file, err := os.Open(gzPath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	// 解压缩gzip
	gzReader, err := gzip.NewReader(file)
	if err != nil {
		return "", err
	}
	defer gzReader.Close()

	// 获取原始文件名（去掉.gz后缀）
	baseName := filepath.Base(gzPath)
	xmlFileName := strings.TrimSuffix(baseName, ".gz")
	if !strings.HasSuffix(strings.ToLower(xmlFileName), ".xml") {
		// 如果去掉.gz后不是xml，自动补充.xml后缀
		xmlFileName += ".xml"
	}

	// 构建目标路径
	targetPath := filepath.Join(destDir, xmlFileName)

	// 创建XML文件并写入解压内容
	outFile, err := os.Create(targetPath)
	if err != nil {
		return "", err
	}
	defer outFile.Close()

	_, err = io.Copy(outFile, gzReader)
	if err != nil {
		return "", err
	}

	return targetPath, nil
}

// 解析EPG XML文件
func parseEPGXML(xmlPath string) (*TV, error) {
	file, err := os.Open(xmlPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	// 解析XML
	var tv TV
	decoder := xml.NewDecoder(file)
	if err := decoder.Decode(&tv); err != nil {
		return nil, err
	}

	return &tv, nil
}

// 构建EPG缓存
func buildEPGCache(tv *TV) error {
	epgCache.mu.Lock()
	defer epgCache.mu.Unlock()

	// 清空旧数据
	epgCache.ChannelMap = make(map[string]string)
	epgCache.ProgramData = make(map[string]map[string][]ProgramItem)

	// 1. 构建频道名称→ID映射
	for _, ch := range tv.Channels {
		// 只取中文名称
		for _, dn := range ch.DisplayName {
			if dn.Lang == "zh" && dn.Value != "" {
				epgCache.ChannelMap[dn.Value] = ch.ID
				break
			}
		}
	}

	// 2. 处理节目数据
	for _, prog := range tv.Programmes {
		// 解析开始时间
		startTime, err := parseEPGTime(prog.Start)
		if err != nil {
			logWarn("解析开始时间失败: %v, 跳过该节目", err)
			continue
		}

		// 解析结束时间
		stopTime, err := parseEPGTime(prog.Stop)
		if err != nil {
			logWarn("解析结束时间失败: %v, 跳过该节目", err)
			continue
		}

		// 格式化日期（YYYY-MM-DD）
		dateStr := startTime.Format("2006-01-02")

		// 格式化时间（HH:MM）
		startStr := startTime.Format("15:04")
		stopStr := stopTime.Format("15:04")

		// 构建节目项
		item := ProgramItem{
			Start: startStr,
			End:   stopStr,
			Title: prog.Title,
		}

		// 初始化层级结构
		if _, ok := epgCache.ProgramData[prog.Channel]; !ok {
			epgCache.ProgramData[prog.Channel] = make(map[string][]ProgramItem)
		}
		if _, ok := epgCache.ProgramData[prog.Channel][dateStr]; !ok {
			epgCache.ProgramData[prog.Channel][dateStr] = make([]ProgramItem, 0)
		}

		// 添加节目
		epgCache.ProgramData[prog.Channel][dateStr] = append(epgCache.ProgramData[prog.Channel][dateStr], item)
	}

	return nil
}

// 解析EPG时间格式（如：20260212010400 +0800）
func parseEPGTime(timeStr string) (time.Time, error) {
	// 分割时间和时区
	parts := strings.Split(timeStr, " ")
	if len(parts) < 1 {
		return time.Time{}, errors.New("无效的时间格式")
	}

	// 解析时间部分（YYYYMMDDHHMMSS）
	baseTime := parts[0]
	if len(baseTime) != 14 {
		return time.Time{}, errors.New("时间格式长度不正确")
	}

	year, _ := strconv.Atoi(baseTime[0:4])
	month, _ := strconv.Atoi(baseTime[4:6])
	day, _ := strconv.Atoi(baseTime[6:8])
	hour, _ := strconv.Atoi(baseTime[8:10])
	minute, _ := strconv.Atoi(baseTime[10:12])
	second, _ := strconv.Atoi(baseTime[12:14])

	// 构建时间
	t := time.Date(year, time.Month(month), day, hour, minute, second, 0, time.Local)
	return t, nil
}

// 保存缓存到文件
func saveCache() error {
	epgCache.mu.RLock()
	defer epgCache.mu.RUnlock()

	data, err := json.Marshal(epgCache)
	if err != nil {
		return err
	}

	return os.WriteFile(config.Cache.CacheFile, data, 0644)
}

// 从文件加载缓存
func loadCache() error {
	if _, err := os.Stat(config.Cache.CacheFile); os.IsNotExist(err) {
		return errors.New("缓存文件不存在")
	}

	data, err := os.ReadFile(config.Cache.CacheFile)
	if err != nil {
		return err
	}

	epgCache.mu.Lock()
	defer epgCache.mu.Unlock()

	return json.Unmarshal(data, epgCache)
}

// HTTP请求处理函数
func epgQueryHandler(w http.ResponseWriter, r *http.Request) {
	logDebug("收到请求: %s %s", r.RemoteAddr, r.RequestURI)

	// 设置响应头
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	// 解析参数
	chName := r.URL.Query().Get("ch")
	dateStr := r.URL.Query().Get("date")

	// 参数校验
	if chName == "" || dateStr == "" {
		errResp := map[string]string{"error": "参数缺失，必须提供ch和date参数"}
		json.NewEncoder(w).Encode(errResp)
		return
	}

	// 验证日期格式
	if _, err := time.Parse("2006-01-02", dateStr); err != nil {
		errResp := map[string]string{"error": "日期格式错误，正确格式为YYYY-MM-DD"}
		json.NewEncoder(w).Encode(errResp)
		return
	}

	// 查询缓存
	epgCache.mu.RLock()
	defer epgCache.mu.RUnlock()

	// 获取频道ID
	chID, ok := epgCache.ChannelMap[chName]
	if !ok {
		errResp := map[string]string{"error": fmt.Sprintf("未找到频道: %s", chName)}
		json.NewEncoder(w).Encode(errResp)
		return
	}

	// 获取节目数据
	datePrograms, ok := epgCache.ProgramData[chID]
	if !ok {
		errResp := map[string]string{"error": fmt.Sprintf("频道%s暂无节目数据", chName)}
		json.NewEncoder(w).Encode(errResp)
		return
	}

	programs, ok := datePrograms[dateStr]
	if !ok {
		errResp := map[string]string{"error": fmt.Sprintf("频道%s在%s暂无节目数据", chName, dateStr)}
		json.NewEncoder(w).Encode(errResp)
		return
	}

	// 构建响应
	resp := EPGResponse{
		ChannelName: chName,
		Date:        dateStr,
		EPGData:     programs,
	}

	// 返回结果
	json.NewEncoder(w).Encode(resp)
}
