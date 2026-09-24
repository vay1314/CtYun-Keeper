package ctyun

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNativeLoginAndTicketUseOneNativeIdentity(t *testing.T) {
	fixedNow := time.UnixMilli(1700000005000)
	loginCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertNativeBaseHeaders(t, r)
		switch r.URL.Path {
		case "/api/auth/client/genChallengeData":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"challengeId": "challenge-id", "challengeCode": "salt"}})
		case "/api/auth/client/login":
			loginCalls++
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if got := r.Form.Get("password"); got != nativeSHA(nativeSHA("password")+"salt") {
				t.Fatalf("native login password = %q", got)
			}
			if r.Form.Get("deviceType") != NativeDeviceType || r.Form.Get("clientVersion") != NativeVersion || r.Form.Get("deviceName") != NativeDeviceName {
				t.Fatalf("native login identity = %v", r.Form)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
				"userId": 123, "userEid": "eid", "tenantId": 456, "secretKey": "secret",
				"commonLoginReqHeader": "common", "bondedDevice": true, "timestamp": fixedNow.UnixMilli(),
			}})
		case "/api/auth/client/getTicket":
			if r.URL.Query().Get("service") != "https://service.test" {
				t.Fatalf("service = %q", r.URL.Query().Get("service"))
			}
			requestID, timestamp := r.Header.Get("CTG-REQUESTID"), r.Header.Get("CTG-TIMESTAMP")
			source := NativeDeviceType + requestID + "456" + timestamp + "123" + NativeVersion + "secret"
			if r.Header.Get("CTG-SIGNATURESTR") != strings.ToUpper(nativeSHA(source)) {
				t.Fatal("native ticket signature is invalid")
			}
			if r.Header.Get("CTG-COMMON-DATA") != "common" || r.Header.Get("x-product-id") != "7" || r.Header.Get("x-client-trace-id") == "" {
				t.Fatalf("native ticket headers = %v", r.Header)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"ticket": "native-ticket"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewNativeClientWithOptions("user", "password", "device-code", nil, NativeOptions{
		APIOrigin: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return fixedNow }, Random: strings.NewReader(strings.Repeat("a", 512)),
	})
	profile, err := client.Login(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if loginCalls != 1 || profile.UserID != 123 || profile.CommonLoginReqHeader != "common" {
		t.Fatalf("login calls=%d profile=%#v", loginCalls, profile)
	}
	ticket, err := client.GetTicket(context.Background(), "https://service.test")
	if err != nil || ticket != "native-ticket" {
		t.Fatalf("GetTicket() = %q, %v", ticket, err)
	}
}

func TestNativeLoginFetchesCaptchaOnlyWhenRequired(t *testing.T) {
	loginCalls, captchaCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/client/genChallengeData":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"challengeId": fmt.Sprintf("challenge-%d", loginCalls), "challengeCode": "salt"}})
		case "/api/auth/client/captcha":
			captchaCalls++
			w.Header().Set("CTG-CAPTCHA-KEY", "captcha-key")
			_, _ = w.Write([]byte("captcha-image"))
		case "/api/auth/client/login":
			loginCalls++
			_ = r.ParseForm()
			if loginCalls == 1 {
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 51040, "msg": "需要验证码"})
				return
			}
			if r.Form.Get("captchaCode") != "ABCD" || r.Form.Get("captchaCodeKey") != "captcha-key" {
				t.Fatalf("captcha fields = %v", r.Form)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"userId": 1, "userEid": "eid", "tenantId": 2, "secretKey": "secret", "commonLoginReqHeader": "common"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	solver := func(_ context.Context, image []byte) (string, error) {
		if string(image) != "captcha-image" {
			t.Fatalf("captcha image = %q", image)
		}
		return "ABCD", nil
	}
	client := NewNativeClientWithOptions("user", "password", "device-code", solver, NativeOptions{APIOrigin: server.URL, HTTPClient: server.Client(), Random: strings.NewReader(strings.Repeat("b", 512))})
	if _, err := client.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	if loginCalls != 2 || captchaCalls != 1 {
		t.Fatalf("login calls=%d captcha calls=%d", loginCalls, captchaCalls)
	}
}

func TestNativePointDetailsUsesOfficialPagingAndTypeFilter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/selforder/api/marketing/userPoints/getPointDetailList" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("pageNum") != "2" || r.URL.Query().Get("pageSize") != "10" || r.URL.Query().Get("msgType") != "2" {
			t.Fatalf("point detail query = %q", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
			"pageNum": 2, "pageSize": 10, "total": 11, "pages": 2, "isLastPage": true,
			"list": []map[string]any{{"logId": 8, "msgType": 2, "remark": "积分兑换", "createDate": 1790190016000, "pointsList": []map[string]any{{"type": 1, "typeDesc": "通用积分", "value": 300}}}},
		}})
	}))
	defer server.Close()
	client := NewNativeClientWithOptions("user", "password", "device", nil, NativeOptions{APIOrigin: server.URL, MarketplaceOrigin: server.URL, HTTPClient: server.Client()})
	client.UseProfile(NativeProfile{UserID: 1, UserEID: "eid", TenantID: 2, SecretKey: "secret", CommonLoginReqHeader: "common"})
	page, err := client.PointDetails(context.Background(), 2, 10, 2)
	if err != nil || page.Page != 2 || page.Total != 11 || len(page.List) != 1 || page.List[0].Points[0].Value != 300 {
		t.Fatalf("PointDetails() = %#v, %v", page, err)
	}
}

func TestNativeMarketplaceUsesNativeIdentityForWholeFlow(t *testing.T) {
	fixedNow := time.UnixMilli(1700000005000)
	orderCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertNativeBaseHeaders(t, r)
		requestID, timestamp := r.Header.Get("CTG-REQUESTID"), r.Header.Get("CTG-TIMESTAMP")
		source := NativeDeviceType + requestID + "456" + timestamp + "123" + NativeVersion + "secret"
		if r.Header.Get("CTG-SIGNATURESTR") != strings.ToUpper(nativeSHA(source)) ||
			r.Header.Get("CTG-COMMON-DATA") != "common" ||
			r.Header.Get("From") != "App-web" || r.Header.Get("x-lang") != "zh-CN" {
			t.Fatalf("native marketplace headers = %v", r.Header)
		}
		switch r.URL.Path {
		case "/selforder/api/marketing/userPoints/getTaskList":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": []map[string]any{{"taskDefId": 1002, "taskDefName": "登录AI云电脑", "status": 2}}})
		case "/selforder/api/marketing/userPoints/getUserPoints":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": []map[string]any{{"pointType": 1, "points": 100, "willOutDate": true}, {"pointType": 1, "points": 900}}})
		case "/selforder/api/selforder/prod/get":
			if r.URL.Query().Get("prodId") != "17000000" || r.URL.Query().Get("prodCode") != "POINTS" {
				t.Fatalf("reward query = %v", r.URL.Query())
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": []map[string]any{{"series": []map[string]any{{"sku": []map[string]any{{"prodId": 99, "prodName": "升配包", "prodType": "pointstplupgrade", "costPoints": 300, "prodStatus": 2}}}}}}})
		case "/selforder/api/desktop/client/pageDesktop":
			if r.Method != http.MethodPost {
				t.Fatalf("desktop method = %s", r.Method)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"desktopList": []map[string]any{{"desktopId": "42", "desktopName": "主机", "prodInstId": "instance-42"}}}})
		case "/selforder/api/desktop-admin/order/mgr/listOrderInstStatisticsV2":
			var queries []struct {
				ProductIDs   []int64 `json:"prodIds"`
				CalendarType string  `json:"calendarType"`
			}
			if err := json.NewDecoder(r.Body).Decode(&queries); err != nil {
				t.Fatal(err)
			}
			if len(queries) != 5 || len(queries[0].ProductIDs) != 1 || queries[0].ProductIDs[0] != 17010101 || queries[0].CalendarType != "2" || queries[4].ProductIDs[0] != 99 {
				t.Fatalf("entitlement statistics request = %#v", queries)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"currentUser": map[string]any{"99": map[string]any{"count": 2}}}})
		case "/selforder/api/selforder/paas/placeOrder":
			orderCalls++
			if r.Method != http.MethodPost {
				t.Fatalf("order method = %s", r.Method)
			}
			var body struct {
				BusinessChannel string `json:"busiChannel"`
				SKU             []struct {
					Attributes []map[string]any `json:"attrs"`
				} `json:"sku"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.BusinessChannel != "010" || len(body.SKU) != 1 ||
				body.SKU[0].Attributes[0]["attrKey"] != "bindDesktopId" ||
				body.SKU[0].Attributes[0]["attrVal"] != "42" {
				t.Fatalf("native order body = %#v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": []map[string]any{{"orderId": "order-id", "orderNo": "order-no", "prodInstId": "instance-id"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewNativeClientWithOptions("user", "password", "device-code", nil, NativeOptions{
		APIOrigin: server.URL, MarketplaceOrigin: server.URL, HTTPClient: server.Client(),
		Now: func() time.Time { return fixedNow }, Random: strings.NewReader(strings.Repeat("c", 2048)),
	})
	client.UseProfile(NativeProfile{UserID: 123, UserEID: "eid", TenantID: 456, SecretKey: "secret", CommonLoginReqHeader: "common"})
	tasks, err := client.Tasks(context.Background())
	if err != nil || len(tasks) != 1 {
		t.Fatalf("Tasks() = %#v, %v", tasks, err)
	}
	points, err := client.Points(context.Background())
	if err != nil || points != 900 {
		t.Fatalf("Points() = %d, %v", points, err)
	}
	rewards, err := client.Rewards(context.Background())
	if err != nil || len(rewards) != 1 {
		t.Fatalf("Rewards() = %#v, %v", rewards, err)
	}
	desktops, err := client.Desktops(context.Background())
	if err != nil || len(desktops) != 1 {
		t.Fatalf("Desktops() = %#v, %v", desktops, err)
	}
	count, err := client.RedemptionStatisticCount(context.Background(), rewards[0])
	if err != nil || count != 2 {
		t.Fatalf("RedemptionStatisticCount() = %d, %v", count, err)
	}
	receipt, err := client.PlaceOrder(context.Background(), rewards[0], 2, desktops[0])
	if err != nil {
		t.Fatal(err)
	}
	if receipt.OrderID != "order-id" || receipt.OrderNo != "order-no" || receipt.ProdInstID != "instance-id" {
		t.Fatalf("PlaceOrder() receipt = %#v", receipt)
	}
	if orderCalls != 1 {
		t.Fatalf("order calls = %d", orderCalls)
	}
}

func assertNativeBaseHeaders(t *testing.T, r *http.Request) {
	t.Helper()
	want := map[string]string{"CTG-DEVICECODE": "device-code", "CTG-DEVICETYPE": NativeDeviceType, "CTG-VERSION": NativeVersion, "CTG-APPMODEL": NativeAppModel, "CTG-APPCHANNEL": NativeAppChannel, "CTG-DEVICE-MODEL": NativeDeviceModel, "CTG-ORIGINALISP": "3"}
	for key, value := range want {
		if got := r.Header.Get(key); got != value {
			t.Fatalf("%s = %q, want %q", key, got, value)
		}
	}
	if _, err := strconv.ParseInt(r.Header.Get("CTG-REQUESTID"), 10, 64); err != nil {
		t.Fatalf("request id: %v", err)
	}
	if _, err := strconv.ParseInt(r.Header.Get("CTG-TIMESTAMP"), 10, 64); err != nil {
		t.Fatalf("timestamp: %v", err)
	}
}
