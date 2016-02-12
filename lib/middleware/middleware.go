package middleware

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"framework-demo/lib/lock"

	jwt "github.com/dgrijalva/jwt-go"
	"github.com/go-xorm/xorm"
	"github.com/gorilla/mux"
	redis "gopkg.in/redis.v3"
)

const (
	DOUBLE_DETECTION_PERIOD = time.Second * 10
	MAX_PROCESS_TIME        = time.Second * 5
)

var (
	db          *xorm.Engine
	redisClient *redis.Client
)

func Init(database *xorm.Engine, client *redis.Client) {
	db = database
	redisClient = client
}

type HandlerWithTx func(r *http.Request, urlValues map[string]string, session *xorm.Session, userId string) (statusCode int, err error, output interface{})
type Handler func(r *http.Request, urlValues map[string]string, db *xorm.Engine, userId string) (statusCode int, err error, output interface{})

type PostHandler func(r io.Reader, urlValues map[string]string, session *xorm.Session, userId string) (statusCode int, err error, output interface{})
type DeleteHandler func(urlValues map[string]string, session *xorm.Session, userId string) (statusCode int, err error, output interface{})

// send a http response to the user with JSON format
func send(res http.ResponseWriter, statusCode int, data interface{}) {
	res.Header().Set("Content-Type", "application/json; charset=utf-8")
	res.WriteHeader(statusCode)
	if d, ok := data.([]byte); ok {
		res.Write(d)
	} else {
		json.NewEncoder(res).Encode(data)
	}
}

type cachedResponse struct {
	StatusCode int
	Err        error
	Output     []byte
}

func DoublePostIntercept(f PostHandler) HandlerWithTx {
	return func(r *http.Request, urlValues map[string]string, session *xorm.Session, userId string) (int, error, interface{}) {
		//FIXME: implement the double request checking

		// split the input stream into two
		buffer := new(bytes.Buffer)
		tee := io.TeeReader(r.Body, buffer)

		//perform hashing on one of the stream
		h := md5.New()
		io.Copy(h, tee)
		md5Hash := hex.EncodeToString(h.Sum(nil))

		//the key is request userId + requestUrl + method + hash of request body
		lockName := userId + `-` + r.URL.Path + `-` + r.Method + `-` + md5Hash + `-LOCK`
		resultName := userId + `-` + r.URL.Path + `-` + r.Method + `-` + md5Hash + `-RESULT`

		//ensure that in case of double request, only one thread can get processed
		if ok, err := lock.AcquireLock(lockName, MAX_PROCESS_TIME, MAX_PROCESS_TIME); err != nil {
			return http.StatusInternalServerError, err, nil
		} else if ok == false {
			return http.StatusConflict, err, nil
		}
		defer lock.ReleaseLock(lockName)

		if b, err := redisClient.Get(resultName).Bytes(); err != nil && err != redis.Nil {
			return http.StatusInternalServerError, err, nil
		} else if err != redis.Nil {
			c := cachedResponse{}
			json.Unmarshal(b, &c)
			return c.StatusCode, c.Err, c.Output
		}

		statusCode, err, output := f(buffer, urlValues, session, userId)
		outputBytes, _ := json.Marshal(output)
		c := cachedResponse{StatusCode: statusCode, Err: err, Output: outputBytes}
		b, _ := json.Marshal(c)
		redisClient.Set(resultName, b, DOUBLE_DETECTION_PERIOD).Result()

		return statusCode, err, output
	}
}

func DoubleDeleteIntercept(f DeleteHandler) HandlerWithTx {
	return func(r *http.Request, urlValues map[string]string, session *xorm.Session, userId string) (int, error, interface{}) {
		//FIXME: implement the double request checking

		return f(urlValues, session, userId)
	}
}

// a middleware to handle user authorization
func AuthAndTx(f HandlerWithTx) http.HandlerFunc {
	return func(res http.ResponseWriter, req *http.Request) {
		userId, statusCode, newTokenString, err := jwtAuth(req)
		if err != nil {
			send(res, statusCode, map[string]string{"error": err.Error()})
			return
		} else {
			res.Header().Add("Authorization", newTokenString) // update JWT Token
		}

		//prepare a database session for the handler
		session := db.NewSession()
		if err := session.Begin(); err != nil {
			send(res, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		defer session.Close()

		//everything seems fine, goto the business logic handler
		if statusCode, err, output := f(req, mux.Vars(req), session, userId); err == nil {
			//the business logic handler return no error, then try to commit the db session
			if err := session.Commit(); err != nil {
				send(res, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			} else {
				send(res, statusCode, output)
			}
		} else {
			session.Rollback()
			send(res, statusCode, map[string]string{"error": err.Error()})
		}
	}
}

// a middleware to handle user authorization
func Auth(f Handler) http.HandlerFunc {
	return func(res http.ResponseWriter, req *http.Request) {
		userId, statusCode, newTokenString, err := jwtAuth(req)
		if err != nil {
			send(res, statusCode, map[string]string{"error": err.Error()})
			return
		} else {
			res.Header().Add("Authorization", newTokenString) // update JWT Token
		}

		//everything seems fine, goto the business logic handler
		if statusCode, err, output := f(req, mux.Vars(req), db, userId); err == nil {
			send(res, statusCode, output)
		} else {
			send(res, statusCode, map[string]string{"error": err.Error()})
		}
	}
}

// a middleware for user authorization which implmented by JWT.
// Please see the documentation: http://jwt.io/
func jwtAuth(req *http.Request) (userId string, statusCode int, newTokenString string, err error) {
	//func Auth(res http.ResponseWriter, req *http.Request, next http.HandlerFunc) {
	// parse and vertify the token string
	tokenString := req.Header.Get("Authorization")
	if len(tokenString) == 0 {
		return ``, http.StatusUnauthorized, "", errors.New("Cannot found HTTP authorization header")
	}

	token, err := jwt.Parse(tokenString, func(t *jwt.Token) (interface{}, error) {
		// make sure the JWT token is using RS256
		if t.Method.Alg() != "HS256" {
			return nil, errors.New("Unexpected signing method")
		}
		//FIXME: use RSA to verify the jwt
		return []byte(`abc123`), nil
	})
	if err != nil {
		return ``, http.StatusUnauthorized, "", nil
	}

	if token.Valid == false { // make sure token is Valid
		return ``, http.StatusUnauthorized, "", errors.New("Wrong jwt token")
	}
	/*
		switch timeStamp := token.Claims["exp"].(type) {
		default:
			return nil, http.StatusUnauthorized, "", errors.New("Improper JWT Token")
		case float64:
			timestamp := time.Unix(int64(timeStamp), 0)
			if timestamp.Before(time.Now()) {
				return nil, http.StatusUnauthorized, "", errors.New("JWT Token has expired")
			}
		}
	*/
	if s, ok := token.Claims["userId"].(string); !ok {
		return ``, http.StatusUnauthorized, "", errors.New("Improper JWT Token")
	} else {
		userId = s
	}

	// let's update the timestamp to up their use time ;)
	token.Claims["exp"] = time.Now().Add(time.Hour * 72).Unix()
	//FIXME: use RSA to protect the hash
	tokenString, _ = token.SignedString([]byte(`abc123`))
	if err != nil {
		return ``, http.StatusInternalServerError, "", errors.New("Problems signing JWT Token")
	}

	return userId, http.StatusOK, tokenString, nil
}
