package ctyun

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	NativeVersion     = "204010003"
	NativeAppVersion  = "4.1.0.656"
	NativeDeviceType  = "25"
	NativeAppModel    = "2"
	NativeAppChannel  = "1020400"
	NativeDeviceName  = "Windows"
	NativeDeviceModel = "windows"
	NativeSysVersion  = "Windows"
)

// NativeProfile is an authentication profile issued for the native Windows
// client identity. It must never be replaced with the Web/Clink Profile.
type NativeProfile struct {
	UserID               int64         `json:"userId"`
	UserEID              string        `json:"userEid"`
	TenantID             int64         `json:"tenantId"`
	SecretKey            string        `json:"secretKey"`
	CommonLoginReqHeader string        `json:"commonLoginReqHeader"`
	UserName             string        `json:"userName"`
	MobilePhone          string        `json:"mobilephone"`
	BondedDevice         bool          `json:"bondedDevice"`
	Offset               time.Duration `json:"offset"`
}

type OrderReceipt struct {
	OrderID    string `json:"orderId"`
	OrderNo    string `json:"orderNo"`
	ProdInstID string `json:"prodInstId"`
}

func (r OrderReceipt) Reference() string {
	if strings.TrimSpace(r.OrderNo) != "" {
		return r.OrderNo
	}
	if strings.TrimSpace(r.OrderID) != "" {
		return r.OrderID
	}
	return r.ProdInstID
}

type NativeOptions struct {
	APIOrigin         string
	MarketplaceOrigin string
	HTTPClient        *http.Client
	Now               func() time.Time
	Random            io.Reader
}

type NativeClient struct {
	username, password, deviceCode string
	solve                          func(context.Context, []byte) (string, error)
	apiOrigin                      string
	marketplaceOrigin              string
	http                           *http.Client
	now                            func() time.Time
	random                         io.Reader
	tick                           atomic.Uint64
	mu                             sync.Mutex
	profileMu                      sync.RWMutex
	profile                        *NativeProfile
}

func NewNativeClient(username, password, deviceCode string, solve func(context.Context, []byte) (string, error)) *NativeClient {
	return NewNativeClientWithOptions(username, password, deviceCode, solve, NativeOptions{})
}

func NewNativeClientWithOptions(username, password, deviceCode string, solve func(context.Context, []byte) (string, error), options NativeOptions) *NativeClient {
	origin := strings.TrimRight(options.APIOrigin, "/")
	if origin == "" {
		origin = PCOrigin
	}
	marketplaceOrigin := strings.TrimRight(options.MarketplaceOrigin, "/")
	if marketplaceOrigin == "" {
		marketplaceOrigin = "https://desk.ctyun.cn"
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		jar, _ := cookiejar.New(nil)
		httpClient = &http.Client{Timeout: 30 * time.Second, Jar: jar}
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	randomSource := options.Random
	if randomSource == nil {
		randomSource = rand.Reader
	}
	return &NativeClient{username: username, password: password, deviceCode: deviceCode, solve: solve, apiOrigin: origin, marketplaceOrigin: marketplaceOrigin, http: httpClient, now: now, random: randomSource}
}

func (c *NativeClient) UseProfile(profile NativeProfile) {
	c.profileMu.Lock()
	c.profile = &profile
	c.profileMu.Unlock()
}

func (c *NativeClient) ClearProfile() {
	c.profileMu.Lock()
	c.profile = nil
	c.profileMu.Unlock()
}

func (c *NativeClient) Profile() (NativeProfile, bool) {
	c.profileMu.RLock()
	defer c.profileMu.RUnlock()
	if c.profile == nil {
		return NativeProfile{}, false
	}
	return *c.profile, true
}

func (c *NativeClient) nextRequestID() string {
	n := c.now().UnixMilli()
	for {
		old := c.tick.Load()
		if n <= int64(old) {
			n = int64(old) + 1
		}
		if c.tick.CompareAndSwap(old, uint64(n)) {
			return strconv.FormatInt(n, 10)
		}
	}
}

func (c *NativeClient) baseHeaders() (http.Header, string, string, error) {
	offset := time.Duration(0)
	if profile, ok := c.Profile(); ok {
		offset = profile.Offset
	}
	timestamp := strconv.FormatInt(c.now().Add(-offset).UnixMilli(), 10)
	requestID := c.nextRequestID()
	randomValue, err := nativeRandom(c.random, 8)
	if err != nil {
		return nil, "", "", err
	}
	traceID, err := nativeUUID(c.random)
	if err != nil {
		return nil, "", "", err
	}
	h := make(http.Header)
	h.Set("CTG-DEVICECODE", c.deviceCode)
	h.Set("CTG-DEVICETYPE", NativeDeviceType)
	h.Set("CTG-REQUESTID", requestID)
	h.Set("CTG-TIMESTAMP", timestamp)
	h.Set("CTG-VERSION", NativeVersion)
	h.Set("CTG-APPMODEL", NativeAppModel)
	h.Set("CTG-APPCHANNEL", NativeAppChannel)
	h.Set("CTG-DEVICE-MODEL", NativeDeviceModel)
	h.Set("CTG-ORIGINALISP", "3")
	h.Set("x-random", randomValue)
	h.Set("x-product-id", "7")
	h.Set("x-client-trace-id", traceID)
	return h, requestID, timestamp, nil
}

func (c *NativeClient) publicHeaders() (http.Header, error) {
	profile, ok := c.Profile()
	if !ok {
		return nil, errors.New("原生客户端尚未登录")
	}
	h, requestID, timestamp, err := c.baseHeaders()
	if err != nil {
		return nil, err
	}
	source := NativeDeviceType + requestID + strconv.FormatInt(profile.TenantID, 10) + timestamp + strconv.FormatInt(profile.UserID, 10) + NativeVersion + profile.SecretKey
	digest := sha256.Sum256([]byte(source))
	h.Set("CTG-TENANTID", strconv.FormatInt(profile.TenantID, 10))
	h.Set("CTG-USERID", strconv.FormatInt(profile.UserID, 10))
	h.Set("CTG-SIGNATURESTR", strings.ToUpper(hex.EncodeToString(digest[:])))
	h.Set("CTG-COMMON-DATA", profile.CommonLoginReqHeader)
	return h, nil
}

type nativeEnvelope struct {
	Code       any             `json:"code"`
	ResultCode any             `json:"resultCode"`
	Msg        string          `json:"msg"`
	Message    string          `json:"message"`
	ResultMsg  string          `json:"resultMsg"`
	Data       json.RawMessage `json:"data"`
}

func decodeNative(response *http.Response, out any) error {
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return err
	}
	var env nativeEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("原生平台返回非 JSON（HTTP %d）", response.StatusCode)
	}
	code := env.Code
	if code == nil {
		code = env.ResultCode
	}
	ok := code == nil || fmt.Sprint(code) == "0" || fmt.Sprint(code) == "200" || fmt.Sprint(code) == "<nil>"
	if response.StatusCode >= http.StatusBadRequest || !ok {
		message := env.Msg
		if message == "" {
			message = env.Message
		}
		if message == "" {
			message = env.ResultMsg
		}
		return APIError{Code: code, Message: message}
	}
	if out != nil && len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("解析原生平台数据：%w", err)
		}
	}
	return nil
}

func (c *NativeClient) do(ctx context.Context, method, endpoint string, body io.Reader, headers http.Header, out any) error {
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	request.Header = headers
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	return decodeNative(response, out)
}

func (c *NativeClient) loginAttempt(ctx context.Context, captchaCode, captchaKey string) (NativeProfile, error) {
	h, _, _, err := c.baseHeaders()
	if err != nil {
		return NativeProfile{}, err
	}
	h.Set("Content-Type", "application/json")
	var challenge struct {
		ChallengeID   string `json:"challengeId"`
		ChallengeCode string `json:"challengeCode"`
	}
	if err := c.do(ctx, http.MethodPost, c.apiOrigin+"/api/auth/client/genChallengeData", strings.NewReader("{}"), h, &challenge); err != nil {
		return NativeProfile{}, err
	}
	if challenge.ChallengeID == "" || challenge.ChallengeCode == "" {
		return NativeProfile{}, errors.New("原生登录 challenge 响应不完整")
	}
	form := url.Values{
		"deviceCode": {c.deviceCode}, "deviceName": {NativeDeviceName}, "deviceType": {NativeDeviceType},
		"deviceModel": {NativeDeviceModel}, "appVersion": {NativeAppVersion}, "sysVersion": {NativeSysVersion},
		"clientVersion": {NativeVersion}, "userAccount": {c.username},
		"password": {nativeSHA(nativeSHA(c.password) + challenge.ChallengeCode)}, "challengeId": {challenge.ChallengeID},
	}
	if captchaCode != "" {
		form.Set("captchaCode", captchaCode)
		form.Set("captchaCodeKey", captchaKey)
	}
	h, _, _, err = c.baseHeaders()
	if err != nil {
		return NativeProfile{}, err
	}
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	var data struct {
		UserID               int64  `json:"userId"`
		UserEID              string `json:"userEid"`
		TenantID             int64  `json:"tenantId"`
		SecretKey            string `json:"secretKey"`
		CommonLoginReqHeader string `json:"commonLoginReqHeader"`
		UserName             string `json:"userName"`
		MobilePhone          string `json:"mobilephone"`
		BondedDevice         bool   `json:"bondedDevice"`
		Timestamp            int64  `json:"timestamp"`
		OffsetTime           int64  `json:"offsetTime"`
	}
	if err := c.do(ctx, http.MethodPost, c.apiOrigin+"/api/auth/client/login", strings.NewReader(form.Encode()), h, &data); err != nil {
		return NativeProfile{}, err
	}
	if data.UserID == 0 || data.UserEID == "" || data.TenantID == 0 || data.SecretKey == "" || data.CommonLoginReqHeader == "" {
		return NativeProfile{}, errors.New("原生登录响应缺少鉴权字段")
	}
	offsetMillis := data.OffsetTime
	if offsetMillis == 0 && data.Timestamp != 0 {
		offsetMillis = c.now().UnixMilli() - data.Timestamp
	}
	return NativeProfile{UserID: data.UserID, UserEID: data.UserEID, TenantID: data.TenantID, SecretKey: data.SecretKey, CommonLoginReqHeader: data.CommonLoginReqHeader, UserName: data.UserName, MobilePhone: data.MobilePhone, BondedDevice: data.BondedDevice, Offset: time.Duration(offsetMillis) * time.Millisecond}, nil
}

func (c *NativeClient) captcha(ctx context.Context) (string, string, error) {
	if c.solve == nil {
		return "", "", errors.New("原生登录需要验证码，但未配置识别服务")
	}
	h, _, _, err := c.baseHeaders()
	if err != nil {
		return "", "", err
	}
	query := url.Values{"width": {"100"}, "height": {"40"}, "userInfo": {c.username}, "_t": {strconv.FormatInt(c.now().UnixMilli(), 10)}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiOrigin+"/api/auth/client/captcha?"+query.Encode(), nil)
	if err != nil {
		return "", "", err
	}
	request.Header = h
	response, err := c.http.Do(request)
	if err != nil {
		return "", "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("获取原生登录验证码 HTTP %d", response.StatusCode)
	}
	image, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return "", "", err
	}
	key := response.Header.Get("CTG-CAPTCHA-KEY")
	if len(image) == 0 || key == "" {
		return "", "", errors.New("原生登录验证码响应不完整")
	}
	code, err := c.solve(ctx, image)
	code = strings.TrimSpace(code)
	if err != nil || code == "" {
		if err == nil {
			err = errors.New("识别结果为空")
		}
		return "", "", fmt.Errorf("原生登录验证码识别失败：%w", err)
	}
	return code, key, nil
}

func (c *NativeClient) Login(ctx context.Context) (NativeProfile, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	captchaCode, captchaKey := "", ""
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		profile, err := c.loginAttempt(ctx, captchaCode, captchaKey)
		if err == nil {
			c.UseProfile(profile)
			return profile, nil
		}
		last = err
		if !requiresNativeCaptcha(err) {
			break
		}
		captchaCode, captchaKey, err = c.captcha(ctx)
		if err != nil {
			return NativeProfile{}, err
		}
	}
	return NativeProfile{}, fmt.Errorf("原生客户端登录失败：%w", last)
}

func (c *NativeClient) GetTicket(ctx context.Context, service string) (string, error) {
	if _, ok := c.Profile(); !ok {
		return "", errors.New("原生客户端尚未登录")
	}
	h, err := c.publicHeaders()
	if err != nil {
		return "", err
	}
	var out struct {
		Ticket string `json:"ticket"`
	}
	endpoint := c.apiOrigin + "/api/auth/client/getTicket?" + url.Values{"service": {service}}.Encode()
	if err := c.do(ctx, http.MethodGet, endpoint, nil, h, &out); err != nil {
		return "", err
	}
	if out.Ticket == "" {
		return "", errors.New("原生 getTicket 未返回 ticket")
	}
	return out.Ticket, nil
}

func (c *NativeClient) marketplace(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	headers, err := c.publicHeaders()
	if err != nil {
		return err
	}
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Pragma", "no-cache")
	headers.Set("Accept", "application/json, text/plain, */*")
	headers.Set("Accept-Language", "zh-CN,zh;q=0.9")
	headers.Set("Content-Type", "application/json")
	headers.Set("From", "App-web")
	headers.Set("Origin", "https://desk.ctyun.cn")
	headers.Set("Referer", "https://desk.ctyun.cn/selforder/points.html")
	headers.Set("Sec-CH-UA", `"Not(A:Brand";v="24", "Chromium";v="122"`)
	headers.Set("Sec-CH-UA-Mobile", "?0")
	headers.Set("Sec-CH-UA-Platform", `"Windows"`)
	headers.Set("User-Agent", "Mozilla/5.0 (Windows NT 6.2; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) QtWebEngine/6.8.0 Chrome/122.0.6261.171 Safari/537.36")
	headers.Set("x-lang", "zh-CN")
	endpoint := c.marketplaceOrigin + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("编码原生积分商城请求：%w", err)
		}
		payload = bytes.NewReader(raw)
	}
	if err := c.do(ctx, method, endpoint, payload, headers, out); err != nil {
		return fmt.Errorf("原生积分商城 %s：%w", path, err)
	}
	return nil
}

func (c *NativeClient) Tasks(ctx context.Context) ([]Task, error) {
	var out []Task
	err := c.marketplace(ctx, http.MethodGet, "/selforder/api/marketing/userPoints/getTaskList", nil, nil, &out)
	return out, err
}

func (c *NativeClient) Points(ctx context.Context) (int, error) {
	var values []pointsBalance
	if err := c.marketplace(ctx, http.MethodGet, "/selforder/api/marketing/userPoints/getUserPoints", nil, nil, &values); err != nil {
		return 0, err
	}
	return mainPointsBalance(values), nil
}

func (c *NativeClient) PointDetails(ctx context.Context, page, pageSize, messageType int) (PointDetailPage, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 50 {
		pageSize = 10
	}
	query := url.Values{"pageNum": {strconv.Itoa(page)}, "pageSize": {strconv.Itoa(pageSize)}}
	if messageType >= 1 && messageType <= 3 {
		query.Set("msgType", strconv.Itoa(messageType))
	}
	var out PointDetailPage
	err := c.marketplace(ctx, http.MethodGet, "/selforder/api/marketing/userPoints/getPointDetailList", query, nil, &out)
	return out, err
}

func (c *NativeClient) Rewards(ctx context.Context) ([]Reward, error) {
	var malls []struct {
		Series []struct {
			Description string `json:"description"`
			SKU         []struct {
				ProductID     int64  `json:"prodId"`
				ProductName   string `json:"prodName"`
				ProductType   string `json:"prodType"`
				Cost          int    `json:"costPoints"`
				PointType     int    `json:"pointType"`
				CostPointType int    `json:"costPointType"`
				Status        int    `json:"prodStatus"`
				Description   string `json:"description"`
				EffectiveAt   any    `json:"effDate"`
				ExpireDate    any    `json:"expireDate"`
			} `json:"sku"`
		} `json:"series"`
	}
	if err := c.marketplace(ctx, http.MethodGet, "/selforder/api/selforder/prod/get", url.Values{"prodId": {"17000000"}, "prodCode": {"POINTS"}}, nil, &malls); err != nil {
		return nil, err
	}
	var out []Reward
	for _, mall := range malls {
		for _, series := range mall.Series {
			for _, sku := range series.SKU {
				description := sku.Description
				if description == "" {
					description = series.Description
				}
				out = append(out, Reward{
					ProductID: sku.ProductID, ProductName: sku.ProductName, ProductType: sku.ProductType,
					Description: description, CostPoints: sku.Cost, PointType: firstNonZero(sku.CostPointType, sku.PointType), Status: sku.Status,
					EffectiveAt: rewardTimeString(sku.EffectiveAt), ExpiresAt: rewardTimeString(sku.ExpireDate),
				})
			}
		}
	}
	return out, nil
}

func (c *NativeClient) Desktops(ctx context.Context) ([]Desktop, error) {
	var page struct {
		DesktopList           []Desktop `json:"desktopList"`
		DesktopPoolList       []Desktop `json:"desktopPoolList"`
		PreemptionDesktopList []Desktop `json:"preemptionDesktopList"`
	}
	if err := c.marketplace(ctx, http.MethodPost, "/selforder/api/desktop/client/pageDesktop", nil, map[string]any{
		"getCnt": 30, "desktopTypes": []string{"1", "2001", "2002", "2003"}, "sortType": "createTimeV1",
	}, &page); err != nil {
		return nil, err
	}
	out := make([]Desktop, 0, len(page.DesktopList)+len(page.DesktopPoolList)+len(page.PreemptionDesktopList))
	out = append(out, page.DesktopList...)
	out = append(out, page.DesktopPoolList...)
	out = append(out, page.PreemptionDesktopList...)
	return out, nil
}

func (c *NativeClient) RedemptionStatisticCount(ctx context.Context, reward Reward) (int, error) {
	queries := []map[string]any{
		{"prodIds": []int64{17010101}, "calendarType": "2"},
		{"prodIds": []int64{17023101}},
		{"prodIds": []int64{17023111}},
		{"prodIds": []int64{17024101}},
	}
	found := false
	for _, query := range queries {
		for _, productID := range query["prodIds"].([]int64) {
			if productID == reward.ProductID {
				found = true
			}
		}
	}
	if !found {
		query := map[string]any{"prodIds": []int64{reward.ProductID}}
		if strings.EqualFold(strings.TrimSpace(reward.ProductType), "cpcai") {
			query["calendarType"] = "2"
		}
		queries = append(queries, query)
	}
	var out struct {
		CurrentUser map[string]struct {
			Count int `json:"count"`
		} `json:"currentUser"`
	}
	if err := c.marketplace(ctx, http.MethodPost, "/selforder/api/desktop-admin/order/mgr/listOrderInstStatisticsV2", nil, queries, &out); err != nil {
		return 0, err
	}
	return out.CurrentUser[strconv.FormatInt(reward.ProductID, 10)].Count, nil
}

func (c *NativeClient) PlaceOrder(ctx context.Context, reward Reward, times int, desktop Desktop) (OrderReceipt, error) {
	if _, ok := c.Profile(); !ok {
		return OrderReceipt{}, errors.New("原生客户端尚未登录")
	}
	if err := validateOrderTarget(reward, times, desktop); err != nil {
		return OrderReceipt{}, err
	}
	var receipts []OrderReceipt
	if err := c.marketplace(ctx, http.MethodPost, "/selforder/api/selforder/paas/placeOrder", nil, buildOrderBody(reward, times, desktop), &receipts); err != nil {
		return OrderReceipt{}, err
	}
	if len(receipts) == 0 {
		return OrderReceipt{}, nil
	}
	return receipts[0], nil
}

func nativeSHA(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func nativeRandom(source io.Reader, length int) (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	raw := make([]byte, length)
	if _, err := io.ReadFull(source, raw); err != nil {
		return "", err
	}
	for i := range raw {
		raw[i] = alphabet[int(raw[i])%len(alphabet)]
	}
	return string(raw), nil
}

func nativeUUID(source io.Reader) (string, error) {
	var raw [16]byte
	if _, err := io.ReadFull(source, raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	value := hex.EncodeToString(raw[:])
	return value[:8] + "-" + value[8:12] + "-" + value[12:16] + "-" + value[16:20] + "-" + value[20:], nil
}

func requiresNativeCaptcha(err error) bool {
	var apiErr APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch fmt.Sprint(apiErr.Code) {
	case "51030", "51031", "51032", "51040":
		return true
	default:
		return false
	}
}
