package main

import (
	"context"
	crand "crypto/rand"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	gsm "github.com/bradleypeabody/gorilla-sessions-memcache"
	_ "github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
	goji "goji.io"
	"goji.io/pat"
	"goji.io/pattern"

	_ "net/http/pprof"
)

var (
	db    *sqlx.DB
	store *gsm.MemcacheStore
)

var (
	templateLogin = template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("login.html")),
	)

	templateRegister = template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("register.html")),
	)

	fmap = template.FuncMap{
		"imageURL": imageURL,
	}
	templateIndex = template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("index.html"),
		getTemplPath("posts.html"),
		getTemplPath("post.html"),
	))

	templateAccountName = template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("user.html"),
		getTemplPath("posts.html"),
		getTemplPath("post.html"),
	))

	templatePosts = template.Must(template.New("posts.html").Funcs(fmap).ParseFiles(
		getTemplPath("posts.html"),
		getTemplPath("post.html"),
	))

	templatePostID = template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("post_id.html"),
		getTemplPath("post.html"),
	))

	templateAdminBanned = template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("banned.html")),
	)
)

const (
	postsPerPage  = 20
	ISO8601Format = "2006-01-02T15:04:05-07:00"
	UploadLimit   = 10 * 1024 * 1024 // 10mb
)

type User struct {
	ID          int       `db:"id"`
	AccountName string    `db:"account_name"`
	Passhash    string    `db:"passhash"`
	Authority   int       `db:"authority"`
	DelFlg      int       `db:"del_flg"`
	CreatedAt   time.Time `db:"created_at"`
}

type Post struct {
	ID           int       `db:"id"`
	UserID       int       `db:"user_id"`
	Imgdata      []byte    `db:"imgdata"`
	Body         string    `db:"body"`
	Mime         string    `db:"mime"`
	CreatedAt    time.Time `db:"created_at"`
	CommentCount int
	Comments     []Comment
	User         User
	CSRFToken    string
}

type Comment struct {
	ID        int       `db:"id"`
	PostID    int       `db:"post_id"`
	UserID    int       `db:"user_id"`
	Comment   string    `db:"comment"`
	CreatedAt time.Time `db:"created_at"`
	User      User
}

func init() {
	memdAddr := os.Getenv("ISUCONP_MEMCACHED_ADDRESS")
	if memdAddr == "" {
		memdAddr = "localhost:11211"
	}
	memcacheClient := memcache.New(memdAddr)
	store = gsm.NewMemcacheStore(memcacheClient, "iscogram_", []byte("sendagaya"))
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
}

func dbInitialize() {
	sqls := []string{
		"DELETE FROM users WHERE id > 1000",
		"DELETE FROM posts WHERE id > 10000",
		"DELETE FROM comments WHERE id > 100000",
		"UPDATE users SET del_flg = 0",
		"UPDATE users SET del_flg = 1 WHERE id % 50 = 0",
	}

	for _, sql := range sqls {
		db.Exec(sql)
	}

	cmd := exec.Command("/home/isucon/private_isu/sql/init.sh")
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stderr
	err := cmd.Run()
	if err != nil {
		log.Fatal(err)
	}
}

func tryLogin(accountName, password string) *User {
	u := User{}
	err := db.Get(&u, "SELECT * FROM users WHERE account_name = ? AND del_flg = 0", accountName)
	if err != nil {
		return nil
	}

	if calculatePasshash(u.AccountName, password) == u.Passhash {
		return &u
	} else {
		return nil
	}
}

func validateUser(accountName, password string) bool {
	return regexp.MustCompile(`\A[0-9a-zA-Z_]{3,}\z`).MatchString(accountName) &&
		regexp.MustCompile(`\A[0-9a-zA-Z_]{6,}\z`).MatchString(password)
}

// ?????????Go????????????????????????????????????????????????????????????????????????OS??????????????????????????????????????????????????????
// ????????????PHP???escapeshellarg?????????????????????????????????
// cf: http://jp2.php.net/manual/ja/function.escapeshellarg.php
func escapeshellarg(arg string) string {
	return "'" + strings.Replace(arg, "'", "'\\''", -1) + "'"
}

func digest(src string) string {
	// openssl????????????????????????????????? (stdin)= ?????????????????????????????????
	out, err := exec.Command("/bin/bash", "-c", `printf "%s" `+escapeshellarg(src)+` | openssl dgst -sha512 | sed 's/^.*= //'`).Output()
	if err != nil {
		log.Print(err)
		return ""
	}

	return strings.TrimSuffix(string(out), "\n")
}

func calculateSalt(accountName string) string {
	return digest(accountName)
}

func calculatePasshash(accountName, password string) string {
	return digest(password + ":" + calculateSalt(accountName))
}

func getSession(r *http.Request) *sessions.Session {
	session, _ := store.Get(r, "isuconp-go.session")

	return session
}

func getSessionUser(r *http.Request) User {
	session := getSession(r)
	uid, ok := session.Values["user_id"]
	if !ok || uid == nil {
		return User{}
	}

	u := User{}

	err := db.Get(&u, "SELECT * FROM `users` WHERE `id` = ?", uid)
	if err != nil {
		return User{}
	}

	return u
}

func getFlash(w http.ResponseWriter, r *http.Request, key string) string {
	session := getSession(r)
	value, ok := session.Values[key]

	if !ok || value == nil {
		return ""
	} else {
		delete(session.Values, key)
		session.Save(r, w)
		return value.(string)
	}
}

type CommentUser struct {
	PostID  int     `db:"post_id"`
	Comment Comment `db:"comment"`
	User    User    `db:"user"`
}

type CommentCount struct {
	PostID int `db:"post_id"`
	Count  int `db:"count"`
}

func makePosts(results []Post, csrfToken string, allComments bool) ([]Post, error) {
	var posts []Post
	var err error

	postIDs := make([]string, len(results))
	for i := range results {
		postIDs[i] = fmt.Sprint(results[i].ID)
	}

	var commentCounts []CommentCount
	err = db.Select(&commentCounts, fmt.Sprintf("SELECT * FROM `comment_count` WHERE `post_id` IN (%s)", strings.Join(postIDs, ",")))
	if err != nil {
		return nil, err
	}
	countMap := make(map[int]int, len(commentCounts))
	for _, c := range commentCounts {
		countMap[c.PostID] = c.Count
	}

	postUserIDs := make([]string, len(results))
	for i := range results {
		postUserIDs[i] = fmt.Sprint(results[i].UserID)
	}

	var postUsers []*User
	err = db.Select(&postUsers, fmt.Sprintf("SELECT * FROM `users` WHERE `id` IN (%s)", strings.Join(postUserIDs, ",")))
	if err != nil {
		return nil, err
	}
	userMap := make(map[int]*User, len(postUsers))
	for _, u := range postUsers {
		userMap[u.ID] = u
	}

	var commentUsers []*CommentUser

	query := fmt.Sprintf("SELECT p.`id` AS `post_id`, c.`id` AS `comment.id`, c.`post_id` AS `comment.post_id`, c.`user_id` AS `comment.user_id`, c.`comment` AS `comment.comment`, c.`created_at` AS `comment.created_at`, u.`id` AS `user.id`, u.`account_name` AS `user.account_name`, u.`passhash` AS `user.passhash`, u.`authority` AS `user.authority`, u.`del_flg` AS `user.del_flg`, u.`created_at` AS `user.created_at` FROM `comments` AS c JOIN `users` AS u ON c.`user_id` = u.`id` JOIN `posts` AS p ON p.`id` = c.`post_id` AND p.`id` IN (%s) ORDER BY c.`created_at` DESC", strings.Join(postIDs, ","))
	if !allComments {
		query += " LIMIT 3"
	}
	err = db.Select(&commentUsers, query)
	if err != nil {
		return nil, err
	}
	postMap := make(map[int][]*CommentUser, len(commentUsers))
	for _, cu := range commentUsers {
		if _, ok := postMap[cu.PostID]; !ok {
			postMap[cu.PostID] = make([]*CommentUser, 0)
		}
		postMap[cu.PostID] = append(postMap[cu.PostID], cu)
	}

	for _, p := range results {
		/*
			err := db.Get(&p.CommentCount, "SELECT `count` FROM `comment_count` WHERE `post_id` = ?", p.ID)
			if err != nil {
				return nil, err
			}
		*/
		p.CommentCount = countMap[p.ID]

		/*
			query := "SELECT c.`id` AS `comment.id`, c.`post_id` AS `comment.post_id`, c.`user_id` AS `comment.user_id`, c.`comment` AS `comment.comment`, c.`created_at` AS `comment.created_at`, u.`id` AS `user.id`, u.`account_name` AS `user.account_name`, u.`passhash` AS `user.passhash`, u.`authority` AS `user.authority`, u.`del_flg` AS `user.del_flg`, u.`created_at` AS `user.created_at` FROM `comments` AS c JOIN `users` AS u ON c.`user_id` = u.`id` WHERE c.`post_id` = ? ORDER BY c.`created_at` DESC"
			if !allComments {
				query += " LIMIT 3"
			}
			var commentUsers []CommentUser
			err = db.Select(&commentUsers, query, p.ID)
			if err != nil {
				return nil, err
			}

			comments := make([]Comment, len(commentUsers))
			for i := range comments {
				comments[i] = commentUsers[i].Comment
				comments[i].User = commentUsers[i].User
			}*/

		postCU := postMap[p.ID]
		comments := make([]Comment, len(postCU))
		for i := range comments {
			comments[i] = postCU[i].Comment
			comments[i].User = postCU[i].User
		}

		/*
			for i := 0; i < len(comments); i++ {
				err := db.Get(&comments[i].User, "SELECT * FROM `users` WHERE `id` = ?", comments[i].UserID)
				if err != nil {
					return nil, err
				}
			}*/

		// reverse
		for i, j := 0, len(comments)-1; i < j; i, j = i+1, j-1 {
			comments[i], comments[j] = comments[j], comments[i]
		}

		p.Comments = comments

		/*
			err = db.Get(&p.User, "SELECT * FROM `users` WHERE `id` = ?", p.UserID)
			if err != nil {
				return nil, err
			}*/
		p.User = *userMap[p.UserID]

		p.CSRFToken = csrfToken

		if p.User.DelFlg == 0 {
			posts = append(posts, p)
		}
		if len(posts) >= postsPerPage {
			break
		}
	}

	return posts, nil
}

func imageURL(p Post) string {
	ext := ""
	if p.Mime == "image/jpeg" {
		ext = ".jpg"
	} else if p.Mime == "image/png" {
		ext = ".png"
	} else if p.Mime == "image/gif" {
		ext = ".gif"
	}

	return "/image/" + strconv.Itoa(p.ID) + ext
}

func isLogin(u User) bool {
	return u.ID != 0
}

func getCSRFToken(r *http.Request) string {
	session := getSession(r)
	csrfToken, ok := session.Values["csrf_token"]
	if !ok {
		return ""
	}
	return csrfToken.(string)
}

func secureRandomStr(b int) string {
	k := make([]byte, b)
	if _, err := crand.Read(k); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", k)
}

func getTemplPath(filename string) string {
	return path.Join("templates", filename)
}

func getExt(mime string) string {
	ext := ""
	if mime == "image/jpeg" {
		ext = "jpg"
	} else if mime == "image/png" {
		ext = "png"
	} else if mime == "image/gif" {
		ext = "gif"
	}
	return ext
}

func getInitialize(w http.ResponseWriter, r *http.Request) {
	dbInitialize()
	/*
		var posts []Post

		err := db.Select(&posts, "SELECT * FROM posts")
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		for _, p := range posts {

			ext := getExt(p.Mime)
			file, err := os.Create(fmt.Sprintf("../public/img/%d.%s", p.ID, ext))
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, err = file.Write(p.Imgdata)
			if err != nil {
				file.Close()
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			file.Close()
		}
	*/
	w.WriteHeader(http.StatusOK)
}

func getLogin(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)

	if isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	templateLogin.Execute(w, struct {
		Me    User
		Flash string
	}{me, getFlash(w, r, "notice")})
}

func postLogin(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	u := tryLogin(r.FormValue("account_name"), r.FormValue("password"))

	if u != nil {
		session := getSession(r)
		session.Values["user_id"] = u.ID
		session.Values["csrf_token"] = secureRandomStr(16)
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
	} else {
		session := getSession(r)
		session.Values["notice"] = "????????????????????????????????????????????????????????????"
		session.Save(r, w)

		http.Redirect(w, r, "/login", http.StatusFound)
	}
}

func getRegister(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	templateRegister.Execute(w, struct {
		Me    User
		Flash string
	}{User{}, getFlash(w, r, "notice")})
}

func postRegister(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	accountName, password := r.FormValue("account_name"), r.FormValue("password")

	validated := validateUser(accountName, password)
	if !validated {
		session := getSession(r)
		session.Values["notice"] = "?????????????????????3?????????????????????????????????6??????????????????????????????????????????"
		session.Save(r, w)

		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	exists := 0
	// ????????????????????????????????????????????????????????????????????????????????????????????????
	db.Get(&exists, "SELECT 1 FROM users WHERE `account_name` = ?", accountName)

	if exists == 1 {
		session := getSession(r)
		session.Values["notice"] = "???????????????????????????????????????????????????"
		session.Save(r, w)

		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	query := "INSERT INTO `users` (`account_name`, `passhash`) VALUES (?,?)"
	result, err := db.Exec(query, accountName, calculatePasshash(accountName, password))
	if err != nil {
		log.Print(err)
		return
	}

	session := getSession(r)
	uid, err := result.LastInsertId()
	if err != nil {
		log.Print(err)
		return
	}
	session.Values["user_id"] = uid
	session.Values["csrf_token"] = secureRandomStr(16)
	session.Save(r, w)

	http.Redirect(w, r, "/", http.StatusFound)
}

func getLogout(w http.ResponseWriter, r *http.Request) {
	session := getSession(r)
	delete(session.Values, "user_id")
	session.Options = &sessions.Options{MaxAge: -1}
	session.Save(r, w)

	http.Redirect(w, r, "/", http.StatusFound)
}

func getIndex(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)

	results := []Post{}

	err := db.Select(&results, fmt.Sprintf("SELECT p.`id`, p.`user_id`, p.`body`, p.`mime`, p.`created_at` FROM `posts` AS p JOIN `users` AS u ON u.id = p.user_id AND u.del_flg = 0 ORDER BY p.`created_at` DESC LIMIT %d", postsPerPage))
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(results, getCSRFToken(r), false)
	if err != nil {
		log.Print(err)
		return
	}

	templateIndex.Execute(w, struct {
		Posts     []Post
		Me        User
		CSRFToken string
		Flash     string
	}{posts, me, getCSRFToken(r), getFlash(w, r, "notice")})
}

func getAccountName(w http.ResponseWriter, r *http.Request) {
	accountName := pat.Param(r, "accountName")
	user := User{}

	err := db.Get(&user, "SELECT * FROM `users` WHERE `account_name` = ? AND `del_flg` = 0", accountName)
	if err != nil {
		log.Print(err)
		return
	}

	if user.ID == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	results := []Post{}

	err = db.Select(&results, "SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` WHERE `user_id` = ? ORDER BY `created_at` DESC", user.ID)
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(results, getCSRFToken(r), false)
	if err != nil {
		log.Print(err)
		return
	}

	commentCount := 0
	err = db.Get(&commentCount, "SELECT COUNT(*) AS count FROM `comments` WHERE `user_id` = ?", user.ID)
	if err != nil {
		log.Print(err)
		return
	}

	postIDs := []int{}
	err = db.Select(&postIDs, "SELECT `id` FROM `posts` WHERE `user_id` = ?", user.ID)
	if err != nil {
		log.Print(err)
		return
	}
	postCount := len(postIDs)

	commentedCount := 0
	if postCount > 0 {
		s := []string{}
		for range postIDs {
			s = append(s, "?")
		}
		placeholder := strings.Join(s, ", ")

		// convert []int -> []interface{}
		args := make([]interface{}, len(postIDs))
		for i, v := range postIDs {
			args[i] = v
		}

		err = db.Get(&commentedCount, "SELECT COUNT(*) AS count FROM `comments` WHERE `post_id` IN ("+placeholder+")", args...)
		if err != nil {
			log.Print(err)
			return
		}
	}

	me := getSessionUser(r)

	templateAccountName.Execute(w, struct {
		Posts          []Post
		User           User
		PostCount      int
		CommentCount   int
		CommentedCount int
		Me             User
	}{posts, user, postCount, commentCount, commentedCount, me})
}

func getPosts(w http.ResponseWriter, r *http.Request) {
	m, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Print(err)
		return
	}
	maxCreatedAt := m.Get("max_created_at")
	if maxCreatedAt == "" {
		return
	}

	t, err := time.Parse(ISO8601Format, maxCreatedAt)
	if err != nil {
		log.Print(err)
		return
	}

	results := []Post{}
	err = db.Select(&results, fmt.Sprintf("SELECT p.`id`, p.`user_id`, p.`body`, p.`mime`, p.`created_at` FROM `posts` AS p JOIN `users` AS u ON u.`id` = p.`user_id` AND u.`del_flg` = 0 WHERE p.`created_at` <= ? ORDER BY p.`created_at` DESC LIMIT %d", postsPerPage), t.Format(ISO8601Format))
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(results, getCSRFToken(r), false)
	if err != nil {
		log.Print(err)
		return
	}

	if len(posts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	templatePosts.Execute(w, posts)
}

func getPostsID(w http.ResponseWriter, r *http.Request) {
	pidStr := pat.Param(r, "id")
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	results := []Post{}
	err = db.Select(&results, "SELECT * FROM `posts` WHERE `id` = ?", pid)
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(results, getCSRFToken(r), true)
	if err != nil {
		log.Print(err)
		return
	}

	if len(posts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	p := posts[0]

	me := getSessionUser(r)
	templatePostID.Execute(w, struct {
		Post Post
		Me   User
	}{p, me})
}

func postIndex(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		session := getSession(r)
		session.Values["notice"] = "?????????????????????"
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	mime := ""
	if file != nil {
		// ?????????Content-Type?????????????????????????????????????????????
		contentType := header.Header["Content-Type"][0]
		if strings.Contains(contentType, "jpeg") {
			mime = "image/jpeg"
		} else if strings.Contains(contentType, "png") {
			mime = "image/png"
		} else if strings.Contains(contentType, "gif") {
			mime = "image/gif"
		} else {
			session := getSession(r)
			session.Values["notice"] = "??????????????????????????????jpg???png???gif????????????"
			session.Save(r, w)

			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
	}

	filedata, err := io.ReadAll(file)
	if err != nil {
		log.Print(err)
		return
	}

	if len(filedata) > UploadLimit {
		session := getSession(r)
		session.Values["notice"] = "??????????????????????????????????????????"
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	tx, err := db.Beginx()
	if err != nil {
		log.Print(err)
		return
	}
	defer tx.Rollback()

	query := "INSERT INTO `posts` (`user_id`, `mime`, `body`) VALUES (?,?,?)"
	result, err := tx.Exec(
		query,
		me.ID,
		mime,
		r.FormValue("body"),
	)
	if err != nil {
		log.Print(err)
		return
	}

	pid, err := result.LastInsertId()
	if err != nil {
		log.Print(err)
		return
	}

	_, err = tx.Exec("INSERT INTO `comment_count` (`post_id`, `count`) VALUES (?, 0)", pid)
	if err != nil {
		log.Print(err)
		return
	}
	tx.Commit()

	imagefile, err := os.Create(fmt.Sprintf("../public/img/%d.%s", pid, getExt(mime)))
	if err != nil {
		log.Print(err)
		return
	}
	defer imagefile.Close()
	imagefile.Write(filedata)

	http.Redirect(w, r, "/posts/"+strconv.FormatInt(pid, 10), http.StatusFound)
}

func getImage(w http.ResponseWriter, r *http.Request) {
	pidStr := pat.Param(r, "id")
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	post := Post{}
	err = db.Get(&post, "SELECT * FROM `posts` WHERE `id` = ?", pid)
	if err != nil {
		log.Print(err)
		return
	}

	ext := pat.Param(r, "ext")

	if ext == "jpg" && post.Mime == "image/jpeg" ||
		ext == "png" && post.Mime == "image/png" ||
		ext == "gif" && post.Mime == "image/gif" {
		w.Header().Set("Content-Type", post.Mime)
		w.Header().Set("Cache-Control", "max-age=3600")
		_, err := w.Write(post.Imgdata)
		if err != nil {
			log.Print(err)
			return
		}
		return
	}

	w.WriteHeader(http.StatusNotFound)
}

func postComment(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	postID, err := strconv.Atoi(r.FormValue("post_id"))
	if err != nil {
		log.Print("post_id?????????????????????")
		return
	}

	tx, err := db.Beginx()
	if err != nil {
		log.Print(err)
		return
	}
	defer tx.Rollback()

	query := "INSERT INTO `comments` (`post_id`, `user_id`, `comment`) VALUES (?,?,?)"
	_, err = tx.Exec(query, postID, me.ID, r.FormValue("comment"))
	if err != nil {
		log.Print(err)
		return
	}

	_, err = tx.Exec("UPDATE `comment_count` SET `count` = `count`+1 WHERE `post_id` = ?", postID)
	if err != nil {
		log.Print(err)
		return
	}
	err = tx.Commit()
	if err != nil {
		log.Print(err)
		return
	}

	http.Redirect(w, r, fmt.Sprintf("/posts/%d", postID), http.StatusFound)
}

func getAdminBanned(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if me.Authority == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	users := []User{}
	err := db.Select(&users, "SELECT * FROM `users` WHERE `authority` = 0 AND `del_flg` = 0 ORDER BY `created_at` DESC")
	if err != nil {
		log.Print(err)
		return
	}

	templateAdminBanned.Execute(w, struct {
		Users     []User
		Me        User
		CSRFToken string
	}{users, me, getCSRFToken(r)})
}

func postAdminBanned(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if me.Authority == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	query := "UPDATE `users` SET `del_flg` = ? WHERE `id` = ?"

	err := r.ParseForm()
	if err != nil {
		log.Print(err)
		return
	}

	for _, id := range r.Form["uid[]"] {
		db.Exec(query, 1, id)
	}

	http.Redirect(w, r, "/admin/banned", http.StatusFound)
}

type RegexpPattern struct {
	regexp *regexp.Regexp
}

func Regexp(reg *regexp.Regexp) *RegexpPattern {
	return &RegexpPattern{regexp: reg}
}

func (reg *RegexpPattern) Match(r *http.Request) *http.Request {
	ctx := r.Context()
	uPath := pattern.Path(ctx)
	if reg.regexp.MatchString(uPath) {
		values := reg.regexp.FindStringSubmatch(uPath)
		keys := reg.regexp.SubexpNames()

		for i := 1; i < len(keys); i++ {
			ctx = context.WithValue(ctx, pattern.Variable(keys[i]), values[i])
		}

		return r.WithContext(ctx)
	}

	return nil
}

func main() {
	go func() {
		log.Print(http.ListenAndServe("localhost:6060", nil))
	}()
	host := os.Getenv("ISUCONP_DB_HOST")
	if host == "" {
		host = "localhost"
	}
	port := os.Getenv("ISUCONP_DB_PORT")
	if port == "" {
		port = "3306"
	}
	_, err := strconv.Atoi(port)
	if err != nil {
		log.Fatalf("Failed to read DB port number from an environment variable ISUCONP_DB_PORT.\nError: %s", err.Error())
	}
	user := os.Getenv("ISUCONP_DB_USER")
	if user == "" {
		user = "root"
	}
	password := os.Getenv("ISUCONP_DB_PASSWORD")
	dbname := os.Getenv("ISUCONP_DB_NAME")
	if dbname == "" {
		dbname = "isuconp"
	}

	dsn := fmt.Sprintf(
		"%s:%s@tcp(%s:%s)/%s?interpolateParams=true&charset=utf8mb4&parseTime=true&loc=Local",
		user,
		password,
		host,
		port,
		dbname,
	)

	db, err = sqlx.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("Failed to connect to DB: %s.", err.Error())
	}
	defer db.Close()

	mux := goji.NewMux()

	mux.HandleFunc(pat.Get("/initialize"), getInitialize)
	mux.HandleFunc(pat.Get("/login"), getLogin)
	mux.HandleFunc(pat.Post("/login"), postLogin)
	mux.HandleFunc(pat.Get("/register"), getRegister)
	mux.HandleFunc(pat.Post("/register"), postRegister)
	mux.HandleFunc(pat.Get("/logout"), getLogout)
	mux.HandleFunc(pat.Get("/"), getIndex)
	mux.HandleFunc(pat.Get("/posts"), getPosts)
	mux.HandleFunc(pat.Get("/posts/:id"), getPostsID)
	mux.HandleFunc(pat.Post("/"), postIndex)
	mux.HandleFunc(pat.Get("/image/:id.:ext"), getImage)
	mux.HandleFunc(pat.Post("/comment"), postComment)
	mux.HandleFunc(pat.Get("/admin/banned"), getAdminBanned)
	mux.HandleFunc(pat.Post("/admin/banned"), postAdminBanned)
	mux.HandleFunc(Regexp(regexp.MustCompile(`^/@(?P<accountName>[a-zA-Z]+)$`)), getAccountName)
	mux.Handle(pat.Get("/*"), http.FileServer(http.Dir("../public")))

	log.Fatal(http.ListenAndServe(":8080", mux))
}
