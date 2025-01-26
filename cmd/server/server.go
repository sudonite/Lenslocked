package main

import (
	"fmt"
	"net/http"
	"os"
	"strconv"

	"github.com/go-chi/chi"
	"github.com/gorilla/csrf"
	"github.com/joho/godotenv"
	"github.com/sudonite/lenslocked/controllers"
	"github.com/sudonite/lenslocked/migrations"
	"github.com/sudonite/lenslocked/models"
	"github.com/sudonite/lenslocked/templates"
	"github.com/sudonite/lenslocked/views"
	"golang.org/x/oauth2"
)

type config struct {
	PSQL models.PostgresConfig
	SMTP models.SMTPConfig
	CSRF struct {
		Key    string
		Secure bool
	}
	Server struct {
		Address string
	}
	OAuthProviders map[string]*oauth2.Config
}

func loadEnvConfig() (config, error) {
	var cfg config
	err := godotenv.Load()
	if err != nil {
		return cfg, err
	}

	cfg.PSQL = models.PostgresConfig{
		Host:     os.Getenv("PSQL_HOST"),
		Port:     os.Getenv("PSQL_PORT"),
		User:     os.Getenv("PSQL_USER"),
		Password: os.Getenv("PSQL_PASSWORD"),
		Database: os.Getenv("PSQL_DATABASE"),
		SSLMode:  os.Getenv("PSQL_SSLMODE"),
	}
	if cfg.PSQL.Host == "" && cfg.PSQL.Port == "" {
		return cfg, fmt.Errorf("No PSQL config provided.")
	}

	cfg.SMTP.Host = os.Getenv("SMTP_HOST")
	portStr := os.Getenv("SMTP_PORT")
	cfg.SMTP.Port, err = strconv.Atoi(portStr)
	if err != nil {
		return cfg, err
	}
	cfg.SMTP.Username = os.Getenv("SMTP_USERNAME")
	cfg.SMTP.Password = os.Getenv("SMTP_PASSWORD")

	cfg.CSRF.Key = os.Getenv("CSRF_KEY")
	cfg.CSRF.Secure = os.Getenv("CSRF_SECURE") == "true"

	cfg.Server.Address = os.Getenv("SERVER_ADDRESS")

	cfg.OAuthProviders = make(map[string]*oauth2.Config)

	dbxConfig := &oauth2.Config{
		ClientID:     os.Getenv("DROPBOX_APP_ID"),
		ClientSecret: os.Getenv("DROPBOX_APP_SECRET"),
		Scopes:       []string{"files.metadata.read", "files.content.read"},
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://www.dropbox.com/oauth2/authorize",
			TokenURL: "https://api.dropboxapi.com/oauth2/token",
		},
	}
	cfg.OAuthProviders["dropbox"] = dbxConfig

	return cfg, nil
}

func run(cfg config) error {
	// Setup the Database
	db, err := models.Open(cfg.PSQL)
	if err != nil {
		return err
	}
	defer db.Close()

	err = db.Ping()
	if err != nil {
		return err
	}

	err = models.MigrateFS(db, migrations.FS, ".")
	if err != nil {
		return err
	}

	// Setup Services
	userService := &models.UserService{
		DB: db,
	}

	sessionService := &models.SessionService{
		DB: db,
	}

	pwResetService := &models.PasswordResetService{
		DB: db,
	}
	galleryService := &models.GalleryService{
		DB: db,
	}

	emailService := models.NewEmailService(cfg.SMTP)

	// Setup Middleware
	umw := controllers.UserMiddleware{
		SessionService: sessionService,
	}

	csrfMw := csrf.Protect([]byte(cfg.CSRF.Key), csrf.Secure(cfg.CSRF.Secure), csrf.Path("/"))

	// Setup Controllers
	usersC := controllers.Users{
		UserService:          userService,
		SessionService:       sessionService,
		PasswordResetService: pwResetService,
		EmailService:         emailService,
	}
	usersC.Templates.New = views.Must(views.ParseFS(templates.FS, "signup.gohtml", "tailwind.gohtml"))
	usersC.Templates.SignIn = views.Must(views.ParseFS(templates.FS, "signin.gohtml", "tailwind.gohtml"))
	usersC.Templates.ForgotPassword = views.Must(views.ParseFS(templates.FS, "forgot-pw.gohtml", "tailwind.gohtml"))
	usersC.Templates.CheckYourEmail = views.Must(views.ParseFS(templates.FS, "check-your-email.gohtml", "tailwind.gohtml"))
	usersC.Templates.ResetPassword = views.Must(views.ParseFS(templates.FS, "reset-pw.gohtml", "tailwind.gohtml"))

	galleriesC := controllers.Galleries{
		GalleryService: galleryService,
	}
	galleriesC.Templates.Index = views.Must(views.ParseFS(templates.FS, "galleries/index.gohtml", "tailwind.gohtml"))
	galleriesC.Templates.New = views.Must(views.ParseFS(templates.FS, "galleries/new.gohtml", "tailwind.gohtml"))
	galleriesC.Templates.Edit = views.Must(views.ParseFS(templates.FS, "galleries/edit.gohtml", "tailwind.gohtml"))
	galleriesC.Templates.Show = views.Must(views.ParseFS(templates.FS, "galleries/show.gohtml", "tailwind.gohtml"))

	oauthC := controllers.OAuth{
		ProviderConfigs: cfg.OAuthProviders,
	}

	// Setup Router and Routes
	r := chi.NewRouter()

	r.Use(csrfMw)
	r.Use(umw.SetUser)

	r.Get("/", controllers.StaticHandler(views.Must(views.ParseFS(
		templates.FS, "home.gohtml", "tailwind.gohtml",
	))))
	r.Get("/contact", controllers.StaticHandler(views.Must(views.ParseFS(
		templates.FS, "contact.gohtml", "tailwind.gohtml",
	))))
	r.Get("/faq", controllers.FAQ(views.Must(views.ParseFS(
		templates.FS, "faq.gohtml", "tailwind.gohtml",
	))))

	r.Get("/signup", usersC.New)
	r.Post("/users", usersC.Create)
	r.Get("/signin", usersC.SignIn)
	r.Post("/signin", usersC.ProccessSignIn)
	r.Post("/signout", usersC.ProccessSignOut)
	r.Get("/forgot-pw", usersC.ForgotPassword)
	r.Post("/forgot-pw", usersC.ProcessForgotPassword)
	r.Get("/reset-pw", usersC.ResetPassword)
	r.Post("/reset-pw", usersC.ProcessResetPassword)
	r.Route("/users/me", func(r chi.Router) {
		r.Use(umw.RequireUser)
		r.Get("/", usersC.CurrentUser)
	})
	r.Route("/galleries", func(r chi.Router) {
		r.Get("/{id}", galleriesC.Show)
		r.Get("/{id}/images/{filename}", galleriesC.Image)

		r.Group(func(r chi.Router) {
			r.Use(umw.RequireUser)
			r.Get("/", galleriesC.Index)
			r.Get("/new", galleriesC.New)
			r.Post("/", galleriesC.Create)
			r.Get("/{id}/edit", galleriesC.Edit)
			r.Post("/{id}", galleriesC.Update)
			r.Post("/{id}/delete", galleriesC.Delete)
			r.Post("/{id}/images", galleriesC.UploadImage)
			r.Post("/{id}/images/url", galleriesC.ImageViaURL)
			r.Post("/{id}/images/{filename}/delete", galleriesC.DeleteImage)
		})
	})
	r.Route("/oauth/{provider}", func(r chi.Router) {
		r.Use(umw.RequireUser)
		r.Get("/connect", oauthC.Connect)
		r.Get("/callback", oauthC.Callback)
	})

	assetHandler := http.FileServer(http.Dir("assets"))
	r.Get("/assets/*", http.StripPrefix("/assets", assetHandler).ServeHTTP)

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Page not found", http.StatusNotFound)
	})

	// Start the Server
	fmt.Printf("Starting the server on %s...\n", cfg.Server.Address)

	return http.ListenAndServe(cfg.Server.Address, r)
}

func main() {
	cfg, err := loadEnvConfig()
	if err != nil {
		panic(err)
	}

	err = run(cfg)
	if err != nil {
		panic(err)
	}
}
