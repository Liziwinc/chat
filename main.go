package main

import (
	"fmt"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	// "time"
	"github.com/SherClockHolmes/webpush-go"
	"github.com/gorilla/websocket"
	_ "modernc.org/sqlite" // Чистый Go драйвер SQLite
)

// Структуры для обмена данными в JSON
type WSMessage struct {
	Type     string   `json:"type"`               // login, chat, init, users
	Username string   `json:"username,omitempty"`
	Password string   `json:"password,omitempty"`
	Text     string   `json:"text,omitempty"`
	Users    []string `json:"users,omitempty"`
}

type ChatMessage struct {
	Username string `json:"username"`
	Text     string `json:"text"`
	Time     string `json:"time"`
}

var db *sql.DB

// Настройки WebSocket
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type PushSubscription struct {
    Endpoint string `json:"endpoint"`
    Keys     struct {
        P256dh string `json:"p256dh"`
        Auth   string `json:"auth"`
    } `json:"keys"`
}

// Hub управляет всеми клиентами
var hub = Hub{
	clients:       make(map[*websocket.Conn]string),
	broadcast:     make(chan WSMessage),
	register:      make(chan *websocket.Conn),
	unregister:    make(chan *websocket.Conn),
	subscriptions: make(map[string]PushSubscription),
}

var hub = Hub{
	clients:    make(map[*websocket.Conn]string),
	broadcast:  make(chan WSMessage),
	register:   make(chan *websocket.Conn),
	unregister: make(chan *websocket.Conn),
}

func initDB() {
	var err error
	db, err = sql.Open("sqlite", "./chat.db")
	if err != nil {
		log.Fatal(err)
	}

	// Создаем таблицы, если их нет (пароли храним в открытом виде ТОЛЬКО для этого примера!)
	createTableSQL := `
	CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT UNIQUE,
		password TEXT
	);
	CREATE TABLE IF NOT EXISTS messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT,
		text TEXT,
		timestamp DATETIME DEFAULT CURRENT_TIMESTAMP
	);`
	_, err = db.Exec(createTableSQL)
	if err != nil {
		log.Fatal("Ошибка создания БД:", err)
	}
}

// Запуск хаба в отдельной горутине
func (h *Hub) run() {
	for {
		select {
		case conn := <-h.register:
			h.mu.Lock()
			h.clients[conn] = "" // Пользователь подключился, но еще не авторизован
			h.mu.Unlock()

		case conn := <-h.unregister:
			h.mu.Lock()
			delete(h.clients, conn)
			h.mu.Unlock()
			conn.Close()
			h.broadcastOnlineUsers()

		case msg := <-h.broadcast:
			h.mu.Lock()
			for conn, username := range h.clients {
				// Рассылаем сообщения только авторизованным (у кого есть username)
				if username != "" {
					conn.WriteJSON(msg)
				}
			}
			h.mu.Unlock()
		}
	}
}

func (h *Hub) broadcastOnlineUsers() {
	var users []string
	h.mu.Lock()
	for _, u := range h.clients {
		if u != "" {
			users = append(users, u)
		}
	}
	h.mu.Unlock()

	msg := WSMessage{Type: "users", Users: users}
	h.mu.Lock()
	for conn, u := range h.clients {
		if u != "" {
			conn.WriteJSON(msg)
		}
	}
	h.mu.Unlock()
}

// Обработка конкретного подключения
func handleConnections(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	hub.register <- conn

	defer func() {
		hub.unregister <- conn
	}()

	for {
		var msg WSMessage
		err := conn.ReadJSON(&msg)
		if err != nil {
			break
		}

		switch msg.Type {
		case "login":
			// Простая логика: если юзера нет - создаем, если есть - проверяем пароль
			var dbPassword string
			err := db.QueryRow("SELECT password FROM users WHERE username = ?", msg.Username).Scan(&dbPassword)
			
			if err == sql.ErrNoRows {
				db.Exec("INSERT INTO users (username, password) VALUES (?, ?)", msg.Username, msg.Password)
			} else if dbPassword != msg.Password {
				conn.WriteJSON(WSMessage{Type: "error", Text: "Неверный пароль"})
				continue
			}

			hub.mu.Lock()
			hub.clients[conn] = msg.Username
			hub.mu.Unlock()

			// Отправляем историю сообщений
			rows, _ := db.Query("SELECT username, text, datetime(timestamp, 'localtime') FROM messages ORDER BY id DESC LIMIT 50")
			var history []ChatMessage
			for rows.Next() {
				var cm ChatMessage
				rows.Scan(&cm.Username, &cm.Text, &cm.Time)
				history = append([]ChatMessage{cm}, history...) // Вставляем в начало, чтобы старые были сверху
			}
			rows.Close()
			
			historyBytes, _ := json.Marshal(history)
			conn.WriteJSON(WSMessage{Type: "init", Text: string(historyBytes)})
			
			hub.broadcastOnlineUsers()

		case "chat":
			hub.mu.Lock()
			sender := hub.clients[conn]
			hub.mu.Unlock()

			if sender != "" {
				db.Exec("INSERT INTO messages (username, text) VALUES (?, ?)", sender, msg.Text)
				hub.broadcast <- WSMessage{
					Type:     "chat",
					Username: sender,
					Text:     msg.Text,
				}

				// --- Логика отправки Push-уведомлений оффлайн пользователям ---
				hub.mu.Lock()
				onlineUsers := make(map[string]bool)
				for _, u := range hub.clients {
					if u != "" {
						onlineUsers[u] = true
					}
				}
				
				// Ищем тех, кто подписан, но не онлайн (и не является отправителем)
				for subUsername := range hub.subscriptions {
					if !onlineUsers[subUsername] && subUsername != sender {
						// Отправляем асинхронно, чтобы не блокировать вебсокет
						go sendPushNotification(subUsername, sender, msg.Text)
					}
				}
				hub.mu.Unlock()
			}
					}
				}
			}

// Обработчик для сохранения подписки на уведомления
func subscribeForPush(w http.ResponseWriter, r *http.Request) {
    var sub PushSubscription
    if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }
    
    // Определяем, какой пользователь подписался (например, из сессии)
    // В нашем упрощенном примере будем передавать username в параметрах
    username := r.URL.Query().Get("username")
    if username == "" {
        http.Error(w, "username is required", http.StatusBadRequest)
        return
    }

    hub.mu.Lock()
    hub.subscriptions[username] = sub
    hub.mu.Unlock()

    w.WriteHeader(http.StatusOK)
}

func sendPushNotification(targetUser string, senderUser string, message string) error {
    hub.mu.Lock()
    sub, ok := hub.subscriptions[targetUser]
    hub.mu.Unlock()
    if !ok {
        return fmt.Errorf("no subscription found")
    }

    s := &webpush.Subscription{
        Endpoint: sub.Endpoint,
        Keys: webpush.Keys{
            P256dh: sub.Keys.P256dh,
            Auth:   sub.Keys.Auth,
        },
    }

    // Формируем JSON для Service Worker'а
    payload, _ := json.Marshal(map[string]string{
        "title": "Новое сообщение от " + senderUser,
        "body":  message,
    })

    // Твои ключи (в реальном проекте вынеси их в .env файл)
    resp, err := webpush.SendNotification(payload, s, &webpush.Options{
        Subscriber:      "mailto:liziwinc@gmail.com", 
        VAPIDPublicKey:  "BMhoQwYNJ40YRNxYLaVXqbOTQmWYBkL9DqL_c38T7bDSTT5mlKGKtf6MQGxvPOPN-14N4G5ZjgR5s8EBa4KGIsM",
        VAPIDPrivateKey: "m2afu3QTlzjZAYZMOgfvVPFh2KetmjymSzIscEps8JM",
        TTL:             30,
    })
    if err != nil {
        return err
    }
    defer resp.Body.Close()
    return nil
}

func main() {
	initDB()
	go hub.run()

	http.HandleFunc("/ws", handleConnections)
	http.HandleFunc("/subscribe", subscribeForPush) // ДОБАВЛЕНО: Регистрация эндпоинта
	http.Handle("/", http.FileServer(http.Dir("./public")))

	log.Println("Сервер запущен на :8080")
	err := http.ListenAndServe(":8080", nil)
	if err != nil {
		log.Fatal("Ошибка сервера: ", err)
	}
}