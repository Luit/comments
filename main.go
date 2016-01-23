package main // import "luit.eu/comments"

// TODO:
//
// Authenticated API for approving/unapproving comments
//
// Akismet ham/spam submit on manual approve/unapprove

// --

// Redis schema:
//
// key {luit.eu/comments}:auto_enable
// value: set of hostnames
// use: SISMEMBER to check if you can add a new :enabled -> "true" key
//
// key: {luit.eu/comments://%s%s}:enabled
// key variables: host, path
// value: github.com/garyburd/redigo/redis.Bool
// note: Key not present means false too.
//
// key {luit.eu/comments://%s%s}:all
// key variables: host, path
// value: zset with timestamps as score and member
// use: ZADD NX for adding, and Z(REV)RANGEBYSCORE for listing
//
// key {luit.eu/comments://%s%s}:approved
// key variables: host, path
// value: zset with timestamps as score and member
// use: ZADD NX for adding, and Z(REV)RANGEBYSCORE for listing, ZREM to mark as spam
//
// key: {luit.eu/comments://%s%s}:comment:%d
// key variables: host, path, timestamp
// value: hash with comment data

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/garyburd/redigo/redis"
)

const (
	keyAutoEnable = "{luit.eu/comments}:auto_enable"
	keyEnabled    = "{luit.eu/comments://%s%s}:enabled"
	keyAll        = "{luit.eu/comments://%s%s}:all"
	keyApproved   = "{luit.eu/comments://%s%s}:approved"
	keyComment    = "{luit.eu/comments://%s%s}:comment:%d"
)

func newPool() *redis.Pool {
	return &redis.Pool{
		MaxIdle:     3,
		IdleTimeout: 240 * time.Second,
		Dial: func() (redis.Conn, error) {
			c, err := redis.Dial("tcp", ":6379")
			if err != nil {
				return nil, err
			}
			return c, err
		},
		TestOnBorrow: func(c redis.Conn, t time.Time) error {
			_, err := c.Do("PING")
			return err
		},
	}
}

var (
	pool *redis.Pool
)

func init() {
	http.HandleFunc("/comments/", commentHandler)
}

func commentHandler(w http.ResponseWriter, r *http.Request) {
	err := r.ParseForm()
	if err != nil {
		http.Error(w, "unable to parse form", http.StatusBadRequest)
	}
	switch r.Method {
	case "GET":
		rawURL := r.FormValue("url")
		u, err := url.Parse(rawURL)
		if err != nil {
			http.Error(w, "bad URL", http.StatusBadRequest)
			return
		}
		conn := pool.Get()
		defer conn.Close()
		comments, err := getComments(conn, u.Host, u.Path)
		if err != nil {
			http.Error(w, "backend error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		e := json.NewEncoder(w)
		e.Encode(comments)
	case "POST":
		req, err := cleanCommentSubmitRequest(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		conn := pool.Get()
		defer conn.Close()
		en, err := autoEnabled(conn, req.host, req.path)
		if err != nil {
			log.Println(err)
			http.Error(w, "backend error", http.StatusInternalServerError)
			return
		}
		if !en {
			http.Error(w, "comments not enabled", http.StatusBadRequest)
			return
		}
		id, err := saveComment(conn, req)
		if err != nil {
			log.Println(err)
			http.Error(w, "backend error", http.StatusInternalServerError)
			return
		}
		approved, err := autoApproveComment(conn, req.host, req.path, id)
		if err != nil {
			log.Println(err)
			// Just the approval that failed, no real harm done
		}
		if approved {
			log.Printf("New approved comment at %s%s: %d\n", req.host, req.path, id)
		} else {
			log.Printf("New unapproved comment at %s%s: %d\n", req.host, req.path, id)
		}
		http.Redirect(w, r, req.Permalink, http.StatusFound)
	}
}

func main() {
	if len(os.Args) > 2 {
		log.Fatal("too many arguments, expecting one or zero")
	}
	addr := "127.0.0.1:2668"
	if len(os.Args) == 2 {
		addr = os.Args[1]
	}
	pool = newPool()
	log.Fatal(http.ListenAndServe(addr, nil))
}

type commentSubmitRequest struct {
	Permalink   string `redis:"permalink"`
	host        string
	path        string
	UserIP      string `redis:"user_ip"`
	UserAgent   string `redis:"user_agent"`
	Referrer    string `redis:"referrer"`
	Author      string `redis:"comment_author"`
	AuthorEmail string `redis:"comment_author_email"`
	AuthorURL   string `redis:"comment_author_url"`
	Content     string `redis:"comment_content"`
}

func cleanCommentSubmitRequest(r *http.Request) (*commentSubmitRequest, error) {
	rawURL := r.FormValue("url")
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Host == "" {
		return nil, errors.New("bad url value")
	}
	if r.FormValue("comment_author") == "" {
		return nil, errors.New("bad comment_author value")
	}
	if r.FormValue("comment_content") == "" {
		return nil, errors.New("bad comment_content value")
	}
	userIP := r.Header.Get("X-Forwarded-For")
	if userIP == "" {
		addr, err := net.ResolveTCPAddr("tcp", r.RemoteAddr)
		if err != nil {
			return nil, err
		}
		userIP = addr.IP.String()
	}
	return &commentSubmitRequest{
		Permalink:   rawURL,
		host:        u.Host,
		path:        u.Path,
		UserIP:      userIP,
		UserAgent:   r.Header.Get("User-Agent"),
		Referrer:    r.Header.Get("Referer"),
		Author:      r.FormValue("comment_author"),
		AuthorEmail: r.FormValue("comment_author_email"),
		AuthorURL:   r.FormValue("comment_author_url"),
		Content:     r.FormValue("comment_content"),
	}, nil
}

// comment contains the part of the data that will be sent through the API
type comment struct {
	ID      string `json:"id" redis:"-"`
	Author  string `json:"author" redis:"comment_author"`
	Content string `json:"content" redis:"comment_content"`
}

func getComments(conn redis.Conn, host, path string) ([]comment, error) {
	ids, err := redis.Strings(conn.Do("ZRANGEBYSCORE",
		fmt.Sprintf(keyApproved, host, path),
		"-inf", "+inf", "LIMIT", "0", "10"))
	if err != nil {
		return nil, err
	}
	comments := make([]comment, 0) // empty list, instead of nil
	for _, id := range ids {
		intid, _ := strconv.ParseInt(id, 10, 64)
		vals, err := redis.Values(conn.Do("HGETALL",
			fmt.Sprintf(keyComment, host, path, intid)))
		if err != nil {
			return nil, err
		}
		var c comment
		if err = redis.ScanStruct(vals, &c); err != nil {
			return nil, err
		}
		c.ID = id
		c.Author = html.EscapeString(c.Author)
		c.Content = html.EscapeString(c.Content)
		comments = append(comments, c)
	}
	return comments, nil
}

func autoEnabled(conn redis.Conn, host, path string) (en bool, err error) {
	en, err = redis.Bool(conn.Do("GET", fmt.Sprintf(keyEnabled, host, path)))
	if err == redis.ErrNil {
		en, err = redis.Bool(conn.Do("SISMEMBER", keyAutoEnable, host))
		if err != nil {
			return
		}
		if en {
			conn.Do("SET", fmt.Sprintf(keyEnabled, host, path), "true")
		}
	}
	return
}

func saveComment(conn redis.Conn, req *commentSubmitRequest) (id int64, err error) {
	for {
		id = time.Now().Unix()
		var added bool
		added, err = redis.Bool(conn.Do("ZADD", fmt.Sprintf(keyAll, req.host, req.path), id, id))
		if err != nil {
			log.Println(err)
			return
		}
		if added {
			break
		}
		time.Sleep(time.Second)
	}
	var ok string
	ok, err = redis.String(conn.Do("HMSET", redis.Args{}.
		Add(fmt.Sprintf(keyComment, req.host, req.path, id)).
		AddFlat(req)...))
	if err != nil {
		return
	}
	if ok != "OK" {
		log.Println("Unexpected return value from HMSET: %q\n", ok)
	}
	return
}

const (
	akismetCheckURL = "https://%s.rest.akismet.com/1.1/comment-check"
)

var (
	akismetKey = os.Getenv("AKISMET_KEY")
)

func autoApproveComment(conn redis.Conn, host, path string, id int64) (bool, error) {
	if akismetKey == "" {
		return false, nil
	}
	values, err := redis.StringMap(conn.Do("HGETALL",
		fmt.Sprintf(keyComment, host, path, id)))
	if err != nil {
		return false, err
	}
	data := url.Values{
		"blog": []string{
			"https://luit.eu/",
		},
	}
	for key, value := range values {
		data.Add(key, value)
	}
	resp, err := http.PostForm(fmt.Sprintf(akismetCheckURL, akismetKey), data)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}
	isSpam, err := strconv.ParseBool(string(body))
	if err != nil {
		return false, errors.New("unexpected return value from akismet: " + string(body))
	}
	if !isSpam {
		added, err := redis.Bool(conn.Do("ZADD", fmt.Sprintf(keyApproved, host, path), id, id))
		if err != nil {
			return false, err
		}
		return added, nil
	}
	return false, nil
}
