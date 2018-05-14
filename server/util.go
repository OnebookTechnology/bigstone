package server

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var osSeparator = string(os.PathSeparator)

func noParameterError(p string) error {
	return errors.New(fmt.Sprintf("Missing parameter '%s' ", p))
}

func checkNecessaryParameters(ctx *gin.Context, args ...string) error {
	for _, arg := range args {
		_, exist := ctx.GetQuery(arg)
		if !exist {
			return noParameterError(arg)
		}
	}
	return nil
}

func nowFormat() string {
	return time.Now().Format("2006-01-02 15:04:05")
}

func nowParse(timeStr string) (time.Time, error) {
	return time.Parse("2006-01-02 15:04:05", timeStr)
}

func nowTimestamp() int64 {
	return time.Now().Unix()
}

func nowTimestampMs() int64 {
	return nowTimestamp() * 1000
}

func doMD5FromString(s string) string {
	hash := md5.New()
	hash.Write([]byte(s))
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func doHmacSHA1(b, sKey []byte) []byte {
	h := hmac.New(sha1.New, []byte(sKey))
	h.Write([]byte(b))
	return h.Sum(nil)
}

func doBase64Encoding(b []byte) string {
	return base64.URLEncoding.EncodeToString(b)
}

func calcSignature(data, sKey string) string {
	return strings.Replace(doBase64Encoding(doHmacSHA1([]byte(data), []byte(sKey))), "=", "~", -1)
}

//return first non-loop local ip address
func getLocalIP() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		if ipNet, ok := a.(*net.IPNet); ok && !ipNet.IP.IsLoopback() {
			if ipNet.IP.To4() != nil {
				return ipNet.IP.String(), nil
			}
		}
	}
	return "", errors.New("No IP address found!")
}

func fnv32(key string) uint32 {
	hash := uint32(2166136261)
	const prime32 = uint32(16777619)
	for i := 0; i < len(key); i++ {
		hash *= prime32
		hash ^= uint32(key[i])
	}
	return hash
}

func genHashDir(key string) string {
	hash := fmt.Sprintf("%x", fnv32(key))
	return hash[0:4] + "/" + hash[4:] + "/"
}

// e.g.
// "1,2,3,4,5" will be converted to [1,2,3,4,5]
func splitCommaToIntArray(idcsStr string) ([]int, error) {
	intArr := []int{}
	splitStr := strings.Split(idcsStr, ",")
	for _, idcStr := range splitStr {
		//Avoid space character
		idcNum, err := strconv.Atoi(strings.TrimSpace(idcStr))
		if err != nil {
			return nil, err
		}
		intArr = append(intArr, idcNum)
	}
	return intArr, nil
}

//filePath must has prefix of os.PathSeparator
//fileDir both has prefix of os.PathSeparator and suffix of os.PathSeparator
//fileName neither
func getFileDirAndName(filePath string) (string, string) {
	if !strings.HasPrefix(filePath, osSeparator) {
		filePath = osSeparator + filePath
	}
	var fileName, fileDir string
	index := strings.LastIndex(filePath, osSeparator)
	fileDir = filePath[:index+1]
	fileName = filePath[index+1:]

	return fileDir, fileName
}

//判断目录是否存在
func isExist(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}

	return true, err
}

//拷贝文件
func CopyFile(dstName, srcName string) (written int64, err error) {
	src, err := os.Open(srcName)
	if err != nil {
		logger.Error("fail to open srcfile: ", srcName)
		return 0, err
	}
	defer src.Close()
	dir := filepath.Dir(dstName)
	isexist, err := isExist(dir)
	if !isexist {
		logger.Error("create dir: ", dir)
		os.MkdirAll(dir, 0755)
	}
	dst, err := os.OpenFile(dstName, os.O_WRONLY|os.O_CREATE, 0755)
	if err != nil {
		logger.Error("fail to open dstfile: ", dstName)
		return 0, err
	}
	defer dst.Close()
	return io.Copy(dst, src)
}

//移动文件
func MoveFile(dstName, srcName string) (movsuccess bool, err error) {
	filelen, err := CopyFile(dstName, srcName)
	if filelen > 0 {
		os.Remove(srcName)
		//logfile.Println("Success to move file: ", srcName, dstName)
		return true, nil
	}
	//logfile.Println("Failed to move file: ", srcName, dstName, err)
	return false, err
}

const digChars = "0123456789"

//生成随机数字.
func RandNumber(num int) string {
	textNum := len(digChars)
	text := ""
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := 0; i < num; i++ {
		text = text + string(digChars[r.Intn(textNum)])
	}
	return text
}

// 计算经纬度
// 返回值的单位为米
func EarthDistance(lat1, lng1, lat2, lng2 float64) float64 {
	radius := float64(6371000) // 6378137
	rad := math.Pi / 180.0
	lat1 = lat1 * rad
	lng1 = lng1 * rad
	lat2 = lat2 * rad
	lng2 = lng2 * rad
	theta := lng2 - lng1
	dist := math.Acos(math.Sin(lat1)*math.Sin(lat2) + math.Cos(lat1)*math.Cos(lat2)*math.Cos(theta))
	return dist * radius
}

//参数转化为map
func getURIParams(uri string) map[string]string {
	kvMap := make(map[string]string)
	paramKVs := strings.Split(uri, "&")
	for _, KWithV := range paramKVs {
		KV := strings.Split(KWithV, "=")
		if len(KV) < 2 {
			continue
		}
		k := KV[0]
		v := KV[1]
		kvMap[k] = v
	}
	return kvMap
}

func getDataReader(ctx *gin.Context) (io.Reader, string, error) {

	var dataReader io.Reader
	var filename string

	//获取上传数据, 判断是否为Form上传
	mpr, err := GetMultipartReader(ctx.Request)

	//非Multipart上传
	if err != nil {
		logger.Error("GetMultipartReader err:", err)
		if err != http.ErrNotMultipart {
			return nil, "", errors.New("Cannot read request body. " + err.Error())
		}

		requestBody := ctx.Request.Body
		dataReader = requestBody
		logger.Warning("Not multipart data. ")
	} else {
		// Multipart上传
		hasMultipartFile := false
		for {
			part, errPart := mpr.GetNextPart()
			if errPart == io.EOF {
				break
			}
			if errPart != nil {
				return nil, "", errors.New("Read part ERROR. " + errPart.Error())
			}
			//if part.FormName() != "" && part.Header.Get("Content-Type") == "application/octet-stream" {
			if part.FormName() != "" {
				filename = part.FileName()
				if filename == "" { //if not file part
					continue
				}
				dataReader = part
				hasMultipartFile = true
				break
			}
		}

		if !hasMultipartFile {
			return nil, "", errors.New("not found multipart file!")
		}

		logger.Info("Formfile filename =", filename)
	}

	return dataReader, filename, nil
}

//表情解码
func UnicodeEmojiDecode(s string) string {
	//emoji表情的数据表达式
	re := regexp.MustCompile("\\[[\\\\u0-9a-zA-Z]+\\]")
	//提取emoji数据表达式
	reg := regexp.MustCompile("\\[\\\\u|]")
	src := re.FindAllString(s, -1)
	for i := 0; i < len(src); i++ {
		e := reg.ReplaceAllString(src[i], "")
		p, err := strconv.ParseInt(e, 16, 32)
		if err == nil {
			s = strings.Replace(s, src[i], string(rune(p)), -1)
		}
	}
	return s
}

//表情转换
func UnicodeEmojiCode(s string) string {
	ret := ""
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		if len(string(rs[i])) == 4 {
			u := `[\u` + strconv.FormatInt(int64(rs[i]), 16) + `]`
			ret += u

		} else {
			ret += string(rs[i])
		}
	}
	return ret
}

//字符串 map 排序, 生成新的参数字符串
func genSortedURIFromMap(m map[string]string) string {
	// To store the keys in slice in sorted order
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// To perform the opertion you want
	str := keys[0] + "=" + m[keys[0]]
	for i, k := range keys {
		if i == 0 {
			continue
		}
		str += "&" + k + "=" + m[k]
	}
	return str
}

func YuanToFen(yuan float64) int {
	fen, _ := strconv.Atoi(fmt.Sprintf("%.0f", yuan*100))
	return fen
}

func FenToYuan(fen int) float64 {
	yuan, _ := strconv.ParseFloat(fmt.Sprintf("%.2f", float64(fen)/100), 64)
	return yuan
}
