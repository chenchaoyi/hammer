package trafficprofiles

import (
	"encoding/json"
	"fmt"
	"io"
  "io/ioutil"
	"log"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
)

type Call struct {
	RandomWeight      float32
	Weight            float32
	URL, Method, Body string
	Type              string // rest or www or "", default it rest

	genFunc func() (string, string) // to generate URL & Body programmically 

	count     int64 // total # of request
	totaltime int64 // total response time.
	backlog   int64
}

func (c *Call) Record(_time int64) {
	atomic.AddInt64(&c.count, 1)
	atomic.AddInt64(&c.totaltime, _time)
}

func (c *Call) Print() string {
	return "API : " + c.Method + "  " + c.URL +
		"\nTotal Call : " + fmt.Sprintf("%d", c.count) +
		"\nResponse Time : " + fmt.Sprintf("%2.4f", float64(c.totaltime)/(float64(c.count)*1.0e9))
}

func (c *Call) normalize() {
	c.Method = strings.ToUpper(c.Method)
	c.Type = strings.ToUpper(c.Type)
}

type Profile struct {
	_totalWeight float32
	_calls       [100]Call
	_num         int
	// _traffic     [100]int64 //to track
}

// beginning fo definition of profile

func (p *Profile) InitProfileFromFile(profileFile string) {
	// to init profile with json stream
	buf := make([]byte, 2048)

	f, _ := os.Open(profileFile)
	f.Read(buf)

	dec := json.NewDecoder(strings.NewReader(string(buf)))
	for {
		var m Call
		if err := dec.Decode(&m); err == io.EOF {
			break
		} else if err != nil {
			//log.Println(err)
			// TODO, fix error handling
			break
		}

		m.normalize()
		p._calls[p._num] = m

		p._totalWeight = p._totalWeight + m.Weight
		p._calls[p._num].RandomWeight = p._totalWeight
		log.Print(p._calls[p._num])

		p._num++
		fmt.Printf("Import Call -> W: %f URL: %s  Method: %s\n", m.Weight, m.URL, m.Method)
	}
}

// to add a new call to traffic profiles
func (p *Profile) addAPI(weight float32, method, url, body string) {
	p._totalWeight = p._totalWeight + weight
	p._calls[p._num].RandomWeight = p._totalWeight
	p._calls[p._num].Method = method
	p._calls[p._num].URL = url
	p._calls[p._num].Body = body

	p._calls[p._num].genFunc = nil

	p._calls[p._num].normalize()

	p._num++
	fmt.Printf("Import Call -> W: %f URL: %s  Method: %s\n", weight, url, method)
}

// to add a new call to traffic profiles with Random Function
func (p *Profile) addAPIFunc(weight float32, method, _type string, genf func() (string, string)) {
	p._totalWeight = p._totalWeight + weight
	p._calls[p._num].RandomWeight = p._totalWeight
	p._calls[p._num].Method = strings.ToUpper(method)
	p._calls[p._num].URL = ""
	p._calls[p._num].Body = ""
	p._calls[p._num].Type = _type

	p._calls[p._num].genFunc = genf

	p._calls[p._num].normalize()

	p._num++
	fmt.Printf("Import Call -> W: %f URL: with func Method: %s\n", weight, method)
}

// print to return a string for web
func (p *Profile) Print() string {
	var x string
	for i := 0; i < p._num; i++ {
		x = x + p._calls[i].Print() + "\n+++++++\n"
	}
	return x
}

// return method, url, body, call
func (p *Profile) NextCall() (string, string, string, string, *Call) {

	r := rand.Float32() * p._totalWeight

	for i := 0; i < p._num; i++ {
		if r <= p._calls[i].RandomWeight {
			if p._calls[i].genFunc != nil {
				u, b := p._calls[i].genFunc()
				return p._calls[i].Method, u, b, p._calls[i].Type, &p._calls[i]
			} else {
				return p._calls[i].Method, p._calls[i].URL, p._calls[i].Body, p._calls[i].Type, &p._calls[i]
			}
		}
	}

	log.Fatal("what? should never reach here")
	return "", "", "", "", &p._calls[1]
}

func (p *Profile) _printProfile() {
	for i := 0; i < p._num; i++ {
		log.Println("Call ", i, " has URL ", p._calls[i].URL, " has TotalWeight ", p._calls[i].RandomWeight)
	}
}

// end of definition of profile

// define profile for Leaderboard event
// return method, url, body, call
func (p *Profile) initLeaderboardEvent() {
	_HOST := "http://leaderboards-us.apps.net" // production

	_GAME := "performance-test"

	// retrieve list of leaderboards
	p.addAPIFunc(5, "GET", "WWW",
		func() (string, string) {
			return _HOST + "/v1/" + _GAME + "/leaderboards", "{}"
		})

	// to get leaderboards a cohort is participating in
	p.addAPIFunc(25, "GET", "WWW",
		func() (string, string) {
			_user := strconv.Itoa(rand.Intn(1000000))
			return _HOST + "/v1/" + _GAME + "/cohorts/cohort-" + _user + "/leaderboards", "{}"
		})

	// to get leaderboard
	p.addAPIFunc(10, "GET", "WWW",
		func() (string, string) {
			_lb := strconv.Itoa(rand.Intn(42))
			return _HOST + "/v1/" + _GAME + "/leaderboards/leaderboard-" + _lb, "{}"
		})

	// to get leaderboards a cohort is participating in
	p.addAPIFunc(10, "PATCH", "WWW",
		func() (string, string) {
			_lb := strconv.Itoa(rand.Intn(42))
			_user := strconv.Itoa(rand.Intn(1000000))
			_score := strconv.Itoa(rand.Int())
			return _HOST + "/v1/" + _GAME + "/leaderboards/leaderboard-" + _lb + "/cohorts/cohort-" + _user,
				"{\"score\":" + _score + "}"
		})
}

func (p *Profile) initRadiationMap(host string) {
  //_HOST := "http://127.0.0.1:8002"

  log.Println(host)
	p.addAPIFunc(5, "POST", "REST",
		func() (string, string) {
			_id := strconv.Itoa(rand.Intn(1000000))
			return host + "/devices", "{\"time\": 0, \"id\": \"" + _id + "\"}"
		})
}

func (p *Profile) initTaxonomy(nodeidFile string, depth int, host string) {
 //content, _ := ioutil.ReadFile("/Users/cchen21/all_node_ids")
 log.Println("node ids file: " + nodeidFile)
 log.Println("max depth: " + strconv.Itoa(depth))
 content, _ := ioutil.ReadFile(nodeidFile)
 lines := strings.Split(string(content), "\n")
 node_num := len(lines)
 log.Println("Found " + strconv.Itoa(node_num) + " node ids")

  _HOST := "http://" + host + "/taxonomy/departments/"

	p.addAPIFunc(65, "GET", "REST",
		func() (string, string) {
      index := rand.Intn(node_num)
      node_id := lines[index]
			return _HOST + node_id, ""
		})

	p.addAPIFunc(5, "GET", "REST",
		func() (string, string) {
      _depth := strconv.Itoa(rand.Intn(depth)+1)
      index := rand.Intn(node_num)
      node_id := lines[index]
			return _HOST + node_id + "?depth=" + _depth, ""
		})

	p.addAPIFunc(20, "GET", "REST",
		func() (string, string) {
			return _HOST, ""
		})

}

//func (p *Profile) Init() {
//func (p *Profile) Init(nodeidFile string, depth int, host string) {
func (p *Profile) Init(host string) {
	// this will need a better way to do this, maybe reflection. TODO
	//p.initLeaderboardEvent()
	//p.initPushNotification()
	p.initRadiationMap(host)
	//p.initTaxonomy(nodeidFile, depth, host)
}

