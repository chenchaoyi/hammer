package main

import (
  "flag"
  "fmt"
  "io"
  "io/ioutil"
  "log"
  "math/rand"
  "net/http"
  "net/url"
  "oauth"
  "runtime"
  "strings"
  // "sync"
  "sync/atomic"
  "time"
  "trafficprofiles"
  "crypto/tls"
)

// to reduce size of thread, speed up
const SizePerThread = 10000000

//var DefaultTransport RoundTripper = &Transport{Proxy: ProxyFromEnvironment}

// Counter will be an atomic, to count the number of request handled
// which will be used to print PPS, etc.
type Counter struct {
  lasttime      int64 // time of last print, in secon, to calculate RPS
  lastcount     int64 // count of last print
  lasttotaltime int64 // last total response time

  count     int64 // total # of request
  totaltime int64 // total response time.

  totalerrors int64 // how many error

  totalslowresp int64 // how many slow response. 

  // to calculate send count
  s_lasttime  int64
  s_lastcount int64
  s_count     int64

  // book keeping just for faster stats report so we do not do it again
  avg_time      float64
  last_avg_time float64
  backlog       int64

  client (*http.Client)

  monitor (*time.Ticker)

  // ideally error should be organized by type TODO
  throttle <-chan time.Time

  runinfo <-chan bool // to indicate current run is good or bad

  // auto find pps
  currentRPS  time.Duration
  lastGoodRPS time.Duration
  lastBadRPS  time.Duration
}

var TrafficProfile = new(trafficprofiles.Profile)
var _DEBUG bool
var _AUTH_METHOD string
var _HOST string

var oauth_client = new(oauth.Client)

// init
func (c *Counter) _init() {
  // init http client
  //c.client = &http.Client{}

  if(proxy != "none") {
    proxyUrl, err := url.Parse(proxy)
    if err != nil {
      log.Fatal(err)
    }

    c.client = &http.Client{
      Transport: &http.Transport{
        DisableKeepAlives:   false,
        MaxIdleConnsPerHost: 200000,
        Proxy: http.ProxyURL(proxyUrl),
        TLSClientConfig: &tls.Config{InsecureSkipVerify : true},
      },
    }
  } else {
    c.client = &http.Client{
      Transport: &http.Transport{
        DisableKeepAlives:   false,
        MaxIdleConnsPerHost: 200000,
        TLSClientConfig: &tls.Config{InsecureSkipVerify : true},
      },
    }
  }

  // make channel for auto finder mode
  c.runinfo = make(chan bool)

  c.monitor = time.NewTicker(time.Second)
  go func() {
    for {
      <-c.monitor.C // rate limit for monitor routine
      go c.pperf()
    }
  }()
}

// increase the count and record response time.
func (c *Counter) record(_time int64) {
  atomic.AddInt64(&c.count, 1)
  atomic.AddInt64(&c.totaltime, _time)

  // if longer that 200ms, it is a slow response
  //if _time > 200000000 {
  if _time > 9000000000 {
    atomic.AddInt64(&c.totalslowresp, 1)
    log.Println("Slow response -> ", float64(_time)/1.0e9)
  }
}

// when error happened, increase counter. maybe add error type later TODO
func (c *Counter) recordError() {
  atomic.AddInt64(&c.totalerrors, 1)

  // we do not record time for errors.
  // and there will not be count incr for calls as well
}

func (c *Counter) recordSend() {
  atomic.AddInt64(&c.s_count, 1)
}

// main goroutine to drive traffic
func (c *Counter) hammer() {
  var req *http.Request
  var err error

  _params := url.Values{}

  t1 := time.Now().UnixNano()

  // before send out, update send count
  c.recordSend()

  _method, _url, _body, _type, _call := TrafficProfile.NextCall()

  req, err = http.NewRequest(_method, _url, strings.NewReader(_body))

  // generate Oauth signatures with body_hash
  switch _AUTH_METHOD {
  case "oauth":
    _signature := oauth_client.AuthorizationHeaderWithBodyHash(nil, _method, _url, _params, _body)
    req.Header.Add("Authorization", _signature)
  }

  if _DEBUG {
    log.Println(req.Header.Get("Authorization"))
  }

  if _method == "PATCH" || _method == "PUT" || _method == "POST" {
    if _type == "REST" {
      // for REST call, we use this one

      // add special haeader for PATCH, PUT and POST
      // _params.Set("Accept", "application/json")
      req.Header.Set("Content-Type", "application/json; charset=utf-8")
      req.Header.Add("X-API-KEY", "b03d027d0eb04697976fb49ef5caf680")
    } else if _type == "WWW" {
      // if thsi is WWW, we use differe content-type
      req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
    }
  }

  res, err := c.client.Do(req)

  response_time := time.Now().UnixNano() - t1

  if err != nil {
    log.Println("Response Time: ", float64(response_time)/1.0e9, " Error: when", _method, _url, "with error ", err)
    c.recordError()
    return
  }

  /*
  ###
  disable reading res.body, no need for our purpose for now,
  by doing this, hope we can save more file descriptor.
  ##
  */
  defer req.Body.Close()
  defer res.Body.Close()

  if _DEBUG {
    data, err := ioutil.ReadAll(res.Body)
    // _b, _ := ioutil.ReadAll(req.Body)
    if err == nil {
      log.Println("Req : ", _method, _url)
      if _AUTH_METHOD != "none" {
        log.Println("Authorization: ", string(req.Header.Get("Authorization")))
      }
      log.Println("Req Body : ", _body)
      log.Println("Response: ", res.Status)
      log.Println("Res Body : ", string(data))
    } else {
      c.recordError()
      return
    }
  }

  // check response code here
  // 409 conflict is ok for PATCH request

  if res.StatusCode >= 400 && res.StatusCode != 409 {
    //fmt.Println(res.Status, string(data))
    log.Println("Got error code --> ", res.Status, "for call ", _method, " ", _url)
    c.recordError()
    return
  }

  // reference --> https://github.com/tenntenn/gae-go-testing/blob/master/recorder_test.go

  // only record time for "good" call
  c.record(response_time)
  _call.Record(response_time)
}

// to print out performance counter
// run every second, will also update last count
func (c *Counter) pperf() {
  sps := c.s_count - c.s_lastcount
  pps := c.count - c.lastcount
  c.backlog = c.s_count - c.count - c.totalerrors

  atomic.StoreInt64(&c.lastcount, c.count)
  atomic.StoreInt64(&c.s_lastcount, c.s_count)

  c.avg_time = float64(c.totaltime) / (float64(c.count) * 1.0e9)
  // c.last_avg_time = TODO!!

  log.Println(" SendPS: ", fmt.Sprintf("%4d", sps),
  " ReceivePS: ", fmt.Sprintf("%4d", pps), fmt.Sprintf("%2.4f", c.avg_time),
  " Pending Requests: ", c.backlog,
  " Error:", c.totalerrors,
  "|", fmt.Sprintf("%2.2f%s", (float64(c.totalerrors)*100.0/float64(c.totalerrors+c.count)), "%"),
  " Slow Ratio: ", fmt.Sprintf("%2.2f%s", (float64(c.totalslowresp)*100.0/float64(c.totalerrors+c.count)), "%"))
}

// routine to return status
func (c *Counter) stats(res http.ResponseWriter, req *http.Request) {
  res.Header().Set(
    "Content-Type", "text/plain",
  )
  io.WriteString(
    res,
    fmt.Sprintf("Total Request: %d\nTotal Error: %d\n==========\n%s",
    c.count, c.totalerrors, string(TrafficProfile.Print())),
  )
}

func index(res http.ResponseWriter, req *http.Request) {
  res.Header().Set(
    "Content-Type",
    "text/html",
  )
  io.WriteString(
    res,
    `test`,
  )
}

func (c *Counter) run_once(pps time.Duration) {
  _interval := 1000000000.0 / pps
  /*
  _send_per_tick := 1
  if pps > 400 {
    _send_per_tick = 5
    _interval = 1000000000.0 * 5 / pps
    log.Println("dount the per tick sending...")
  }
  */
  c.throttle = time.Tick(_interval * time.Nanosecond)

  // fmt.println _users

  go func() {
    for {
      <-c.throttle // rate limit our Service.Method RPCs
      go c.hammer()
      /*
      if _send_per_tick > 1 {
        // send two per tick for very high RPS to be more accurate
        go c.hammer()
        go c.hammer()
        go c.hammer()
        go c.hammer()
      }
      */
    }
  }()
}

func (c *Counter) findPPS(_p int64) {
  var _rps time.Duration

  _rps = time.Duration(_p)
  log.Println(_rps)
  // already a gorouting, we just do a infinity loop to find the best RPS
  for {
    c.run_once(_rps)
    log.Println("Run RPS -> ", int(_rps))
    _result := <-c.runinfo
    log.Println(_result)

    // now we know pass of failed, we can start adjust _rps
    if _result {
      // first, we want make sure we can exit the run, that is
      // if the good and failed RPS is within 5 RPS (will change
      // to 5% later), we can assume we found what we are looking for
      c.lastGoodRPS = _rps
      if (c.lastGoodRPS*c.lastBadRPS > 0) && (c.lastGoodRPS-c.lastBadRPS < 5) {
        log.Println("found it!", _rps)
        // additional report and then quit the process
      }
    } else {
      c.lastBadRPS = _rps
    }
    // not found, keep running, next RPS will be (good + bad ) / 2
    if c.lastBadRPS == 0 {
      _rps = c.lastGoodRPS * 2
    } else if c.lastGoodRPS == 0 {
      _rps = c.lastGoodRPS / 2
    } else {
      _rps = (c.lastGoodRPS + c.lastBadRPS) / 2
    }
  }
}

// init the program from command line
var initRPS int64
var profileFile string
var initKey string
var initSecret string
var nodeidFile string
var host string
var proxy string

func init() {
  //var env string

  flag.Int64Var(&initRPS, "rps", 100, "set RPS")
  flag.StringVar(&profileFile, "profile", "", "traffic profile")
  flag.StringVar(&host, "host", "api.mobile.walmart.com", "server host address")
  flag.BoolVar(&_DEBUG, "debug", false, "set debug flag")
  flag.StringVar(&_AUTH_METHOD, "auth", "none", "set authorization flag (oauth|none)")
  flag.StringVar(&proxy, "proxy", "none", "Set HTTP proxy (need to specify scheme. e.g. http://127.0.0.1:8888)")
  flag.StringVar(&initKey, "oauthkey", "8e83a8372268", "set oauth key")
  flag.StringVar(&initSecret, "oauthsecret", "24d643594f7cf03a52f5f6fe7c1b60dd", "set oauth secret")
  //flag.StringVar(&env, "env", "staging", "set testing environment")

  oauth_client.Credentials.Token = initKey
  oauth_client.Credentials.Secret = initSecret

  _AUTH_METHOD = strings.ToLower(_AUTH_METHOD)
}

// main func
func main() {
  // rate_per_sec := 10
  // throttle := time.Tick( 15 * time.Millisecond)
  // const NCPU = 16

  NCPU := runtime.NumCPU()
  log.Println("# of CPU is ", NCPU)

  runtime.GOMAXPROCS(NCPU + 3)

  flag.Parse()
  log.Println("proxy -> ", proxy)
  log.Println("RPS is", initRPS)

  if profileFile != "" {
    log.Println("Profile is", profileFile)
    TrafficProfile.InitProfileFromFile(profileFile)
  } else {
    TrafficProfile.Init(host)
    //TrafficProfile.Init(nodeidFile, depth, host)
  }

  rand.Seed(time.Now().UnixNano())

  counter := new(Counter)
  counter._init()

  go func() {
    counter.findPPS(initRPS)
  }()

  // start web interface here, which returns only stats in text format
  http.HandleFunc("/", index)
  http.HandleFunc("/test", func(res http.ResponseWriter, req *http.Request) {
    log.Println("receive request")
    counter.stats(res, req)
  })
  http.ListenAndServe(":9001", nil)

  // this will block the program, we may add a targe # of msg here.
  var input string
  fmt.Scanln(&input)
}
