package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"reflect"
	"strconv"

	"cloud.google.com/go/storage"
	jwtmiddleware "github.com/auth0/go-jwt-middleware"
	jwt "github.com/dgrijalva/jwt-go"
	"github.com/gorilla/mux"
	"github.com/olivere/elastic"
	"github.com/pborman/uuid"
)

const (
	POST_INDEX = "post"
	DISTANCE   = "200km"

	ES_URL      = "http://10.128.0.2:9200"
	BUCKET_NAME = "around-probe"
	//TODO: add API Version Prefix
	//If added, then we need to include the API_PREFIX when testing
	//API_PREFIX = "/api/v1"
)

var (
	mediaTypes = map[string]string{
		".jpeg": "image",
		".jpg":  "image",
		".gif":  "image",
		".png":  "image",
		".mov":  "video",
		".mp4":  "video",
		".avi":  "video",
		".flv":  "video",
		".wmv":  "video",
	}
)

type Location struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

type Post struct {
	// `json:"user"` is for the json parsing of this User field. Otherwise, by default it's 'User'.
	User     string   `json:"user"`
	Message  string   `json:"message"`
	Location Location `json:"location"`
	Url      string   `json:"url"`
	Type     string   `json:"type"`
	Face     float32  `json:"face"`
}

func main() {
	fmt.Println("started-service")

	r := mux.NewRouter()

	//Update 01:so now the api is RESTful
	//If the request is not following the rule, it would return error 405
	/*
		r.Handle("/post", http.HandlerFunc(handlerPost)).Methods("POST", "OPTIONS")
		r.Handle("/search", http.HandlerFunc(handlerSearch)).Methods("GET", "OPTIONS")
		r.Handle("/cluster", http.HandlerFunc(handlerCluster)).Methods("GET", "OPTIONS")
	*/

	//Update 02: use jwt middleware to protect the apis
	//create jwt middleware using the secret key and encryption algorithm
	jwtMiddleware := jwtmiddleware.New(jwtmiddleware.Options{
		ValidationKeyGetter: func(token *jwt.Token) (interface{}, error) {
			return []byte(mySigningKey), nil
		},
		SigningMethod: jwt.SigningMethodHS256,
	})
	//wrap the request using middleware
	r.Handle("/post", jwtMiddleware.Handler(http.HandlerFunc(handlerPost))).Methods("POST", "OPTIONS")
	r.Handle("/search", jwtMiddleware.Handler(http.HandlerFunc(handlerSearch))).Methods("GET", "OPTIONS")
	r.Handle("/cluster", jwtMiddleware.Handler(http.HandlerFunc(handlerCluster))).Methods("GET", "OPTIONS")
	r.Handle("/signup", http.HandlerFunc(handlerSignup)).Methods("POST", "OPTIONS")
	r.Handle("/login", http.HandlerFunc(handlerLogin)).Methods("POST", "OPTIONS")

	log.Fatal(http.ListenAndServe(":8080", r))

}

func handlerPost(w http.ResponseWriter, r *http.Request) {
	fmt.Println("received one post request")

	w.Header().Set("Content-type", "application/json")

	//Allow the request from anywhere
	w.Header().Set("Access-Control-Allow-Origin", "*")
	//Support the login
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization")
	//Early return for options request
	//options request is a special request send by browser to ensure the API actually exists
	//after receiving return from the server the browser would send the true request
	if r.Method == "OPTIONS" {
		return
	}

	user := r.Context().Value("user")
	claims := user.(*jwt.Token).Claims
	username := claims.(jwt.MapClaims)["username"]

	/*Read parameter from client*/
	lat, _ := strconv.ParseFloat(r.FormValue("lat"), 64)
	lon, _ := strconv.ParseFloat(r.FormValue("lon"), 64)

	p := &Post{
		User:    username.(string),
		Message: r.FormValue("message"),
		Location: Location{
			Lat: lat,
			Lon: lon,
		},
	}

	/*Save image to GCS*/
	file, header, err := r.FormFile("image")
	if err != nil {
		http.Error(w, "Image is not available", http.StatusBadRequest)
		fmt.Printf("Image is not available %v\n", err)
		return
	}

	//check the type of the file
	suffix := filepath.Ext(header.Filename)
	if t, ok := mediaTypes[suffix]; ok {
		p.Type = t
	} else {
		p.Type = "unknown"
	}
	//save and get the link created
	id := uuid.New()
	mediaLink, err := saveToGCS(file, id)
	if err != nil {
		http.Error(w, "Failed to save image to GCS", http.StatusInternalServerError)
		fmt.Printf("Failed to save image to GCS %v\n", err)
		return
	}
	p.Url = mediaLink

	/*annotate image with vision api*/
	if p.Type == "image" {
		uri := fmt.Sprintf("gs://%s/%s", BUCKET_NAME, id)
		if score, err := annotate(uri); err != nil {
			http.Error(w, "Failed to annotate image", http.StatusInternalServerError)
			fmt.Printf("Failed to annotate the image %v\n", err)
			return
		} else {
			p.Face = score
		}
	}

	/*save post to ES*/
	err = saveToES(p, POST_INDEX, id)
	if err != nil {
		http.Error(w, "Failed to save post to Elasticsearch", http.StatusInternalServerError)
		fmt.Printf("Failed to save post to Elasticsearch %v\n", err)
		return
	}

}

func handlerCluster(w http.ResponseWriter, r *http.Request) {
	fmt.Println("Received one cluster request")
	w.Header().Set("Content-Type", "application/json")

	//Allow the request from anywhere
	w.Header().Set("Access-Control-Allow-Origin", "*")
	//Support the login
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization")
	//Early return for options request
	//options request is a special request send by browser to ensure the API actually exists
	//after receiving return from the server the browser would send the true request
	if r.Method == "OPTIONS" {
		return
	}

	term := r.URL.Query().Get("term")
	query := elastic.NewRangeQuery(term).Gte(0.9)

	searchResult, err := readFromES(query, POST_INDEX)
	if err != nil {
		http.Error(w, "Failed to read from Elasticsearch", http.StatusInternalServerError)
		return
	}

	posts := getPostFromSearchResult(searchResult)
	js, err := json.Marshal(posts)
	if err != nil {
		http.Error(w, "Failed to parse post object", http.StatusInternalServerError)
		fmt.Printf("Failed to parse post object %v\n", err)
		return
	}
	w.Write(js)
}

func handlerSearch(w http.ResponseWriter, r *http.Request) {
	fmt.Println("Received one search request")

	w.Header().Set("Content-Type", "application/json")

	//Allow the request from anywhere
	w.Header().Set("Access-Control-Allow-Origin", "*")
	//Support the login
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization")
	//Early return for options request
	//options request is a special request send by browser to ensure the API actually exists
	//after receiving return from the server the browser would send the true request
	if r.Method == "OPTIONS" {
		return
	}

	//we did not catch the error because we know the front end would only pass in correct value (num)
	lat, _ := strconv.ParseFloat(r.URL.Query().Get("lat"), 64)
	lon, _ := strconv.ParseFloat(r.URL.Query().Get("lon"), 64)
	//range is optional
	ran := DISTANCE
	if val := r.URL.Query().Get("range"); val != "" {
		ran = val + "km"
	}
	//fmt.Println("range is", ran)

	query := elastic.NewGeoDistanceQuery("location")
	query = query.Distance(ran).Lat(lat).Lon(lon)
	searchResult, err := readFromES(query, POST_INDEX)
	if err != nil {
		http.Error(w, "Failed to read post from Elasticsearch", http.StatusInternalServerError)
		fmt.Printf("Failed to read post from Elasticsearch %v.\n", err)
		return
	}
	posts := getPostFromSearchResult(searchResult)

	js, err := json.Marshal(posts)
	if err != nil {
		http.Error(w, "Failed to parse posts into JSON format", http.StatusInternalServerError)
		fmt.Printf("Failed to parse posts into JSON format %v.\n", err)
		return
	}
	w.Write(js)
}

func readFromES(query elastic.Query, index string) (*elastic.SearchResult, error) {
	client, err := elastic.NewClient(elastic.SetURL(ES_URL))
	if err != nil {
		return nil, err
	}

	searchResult, err := client.Search().
		Index(index).
		Query(query).
		Pretty(true).
		Do(context.Background())
	if err != nil {
		return nil, err
	}

	return searchResult, nil
}

func getPostFromSearchResult(searchResult *elastic.SearchResult) []Post {
	var ptype Post
	var posts []Post

	for _, item := range searchResult.Each(reflect.TypeOf(ptype)) {
		p := item.(Post)
		posts = append(posts, p)
	}
	return posts
}

func saveToGCS(r io.Reader, objectName string) (string, error) {
	ctx := context.Background()
	client, err := storage.NewClient(ctx)
	if err != nil {
		return "", err
	}

	bucket := client.Bucket(BUCKET_NAME)
	if _, err := bucket.Attrs(ctx); err != nil {
		return "", err
	}

	object := bucket.Object(objectName)
	wc := object.NewWriter(ctx)
	if _, err := io.Copy(wc, r); err != nil {
		return "", err
	}
	if err := wc.Close(); err != nil {
		return "", err
	}

	if err := object.ACL().Set(ctx, storage.AllUsers, storage.RoleReader); err != nil {
		return "", err
	}

	attrs, err := object.Attrs(ctx)
	if err != nil {
		return "", err
	}

	fmt.Printf("Image is saved to GCS: %s\n", attrs.MediaLink)
	return attrs.MediaLink, nil
}

//TODO: change the code to only support post and user
func saveToES(i interface{}, index string, id string) error {
	client, err := elastic.NewClient(elastic.SetURL(ES_URL))
	if err != nil {
		return err
	}

	_, err = client.Index().
		Index(index).
		Id(id).
		BodyJson(i).
		Do(context.Background())

	if err != nil {
		return err
	}

	return nil
}
