Привет! Отличная задача. Как разработчик и сисадмин, я полностью одобряю твой выбор стека: Go идеально подходит для вебсокетов благодаря легковесным горутинам, а `modernc.org/sqlite` избавляет от головной боли с зависимостями CGO при кросс-компиляции. 

Поскольку у тебя уже есть инженерный бэкграунд и понимание фронтенда, я не буду разжевывать базовые концепции HTML/CSS, а сфокусируюсь на специфике Go (каналы, горутины) и инфраструктурной части.

Мы разобьем проект на 4 шага: бэкенд, фронтенд, локальный запуск и вывод в интернет через Cloudflare.

---

### Шаг 1: Бэкенд на Go (`main.go`)

Здесь реализован паттерн **Hub** (Центр обработки сообщений). В Go принято не блокировать основной поток, поэтому каждое WebSocket-соединение будет обслуживаться двумя отдельными горутинами (одна читает, другая пишет).

Создай файл `main.go` и вставь этот код:

```go
package main

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

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

// Hub управляет всеми клиентами
type Hub struct {
	clients    map[*websocket.Conn]string // conn -> username
	broadcast  chan WSMessage
	register   chan *websocket.Conn
	unregister chan *websocket.Conn
	mu         sync.Mutex // Мьютекс для безопасного доступа к мапе
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
			}
		}
	}
}

func main() {
	initDB()
	go hub.run() // Запускаем хаб в фоне

	http.HandleFunc("/ws", handleConnections)
	http.Handle("/", http.FileServer(http.Dir("./public"))) // Отдаем статику из папки public

	log.Println("Сервер запущен на :8080")
	err := http.ListenAndServe(":8080", nil)
	if err != nil {
		log.Fatal("Ошибка сервера: ", err)
	}
}
```

---

### Шаг 2: Простой Frontend (`index.html`)

Стиль минималистичный, структура адаптивная. Создай папку `public` рядом с `main.go`, а внутри неё файл `index.html`:

```html
<!DOCTYPE html>
<html lang="ru">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Minimal Messenger</title>
    <style>
        * { margin: 0; padding: 0; box-sizing: border-box; font-family: 'Segoe UI', Tahoma, Geneva, Verdana, sans-serif; }
        body { background-color: #F5F5F5; display: flex; height: 100vh; justify-content: center; align-items: center; }
        #auth-screen { background: white; padding: 30px; border-radius: 8px; box-shadow: 0 4px 12px rgba(0,0,0,0.1); width: 300px; text-align: center; }
        #auth-screen input { width: 100%; padding: 10px; margin: 10px 0; border: 1px solid #ccc; border-radius: 4px; }
        button { background: #2196F3; color: white; border: none; padding: 10px 20px; border-radius: 4px; cursor: pointer; width: 100%; transition: background 0.3s; }
        button:hover { background: #1E88E5; }
        #chat-screen { display: none; width: 100%; max-width: 800px; height: 90vh; background: white; box-shadow: 0 4px 12px rgba(0,0,0,0.1); border-radius: 8px; flex-direction: row; overflow: hidden; }
        #sidebar { width: 250px; background: #2c3e50; color: white; padding: 20px; overflow-y: auto; }
        #main-chat { flex: 1; display: flex; flex-direction: column; }
        #messages { flex: 1; padding: 20px; overflow-y: auto; display: flex; flex-direction: column; gap: 10px; }
        .message { background: #e3f2fd; padding: 10px; border-radius: 8px; align-self: flex-start; max-width: 80%; }
        .message.my { background: #bbdefb; align-self: flex-end; }
        .meta { font-size: 0.8em; color: #666; margin-bottom: 4px; display: block; }
        #input-area { display: flex; padding: 15px; background: #eee; border-top: 1px solid #ccc; }
        #message-input { flex: 1; padding: 10px; border: 1px solid #ccc; border-radius: 4px; margin-right: 10px; }
        #input-area button { width: auto; }
    </style>
</head>
<body>

    <div id="auth-screen">
        <h2>Вход в чат</h2>
        <input type="text" id="username" placeholder="Логин">
        <input type="password" id="password" placeholder="Пароль">
        <button onclick="login()">Войти</button>
    </div>

    <div id="chat-screen">
        <div id="sidebar">
            <h3>В сети:</h3>
            <ul id="online-users" style="list-style: none; margin-top: 15px; color: #ecf0f1;"></ul>
        </div>
        <div id="main-chat">
            <div id="messages"></div>
            <div id="input-area">
                <input type="text" id="message-input" placeholder="Введите сообщение..." onkeypress="if(event.key === 'Enter') sendMessage()">
                <button onclick="sendMessage()">Отправить</button>
            </div>
        </div>
    </div>

    <script>
        let ws;
        let myUsername = "";

        function connectWebSocket() {
            // Определяем протокол в зависимости от того, HTTPS мы или нет (для Cloudflare)
            const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
            ws = new WebSocket(`${protocol}//${window.location.host}/ws`);

            ws.onmessage = function(event) {
                const msg = JSON.parse(event.data);
                
                if (msg.type === "error") {
                    alert(msg.text);
                    window.location.reload();
                } else if (msg.type === "init") {
                    document.getElementById('auth-screen').style.display = 'none';
                    document.getElementById('chat-screen').style.display = 'flex';
                    const history = JSON.parse(msg.text);
                    history.forEach(m => renderMessage(m.username, m.text, m.time));
                } else if (msg.type === "chat") {
                    renderMessage(msg.username, msg.text, new Date().toLocaleTimeString());
                } else if (msg.type === "users") {
                    const ul = document.getElementById('online-users');
                    ul.innerHTML = '';
                    msg.users.forEach(u => {
                        const li = document.createElement('li');
                        li.textContent = "🟢 " + u;
                        li.style.marginBottom = "8px";
                        ul.appendChild(li);
                    });
                }
            };

            ws.onclose = () => {
                alert("Соединение разорвано. Перезагрузите страницу.");
            };
        }

        function login() {
            const user = document.getElementById('username').value.trim();
            const pass = document.getElementById('password').value.trim();
            if (!user || !pass) return alert("Введите логин и пароль");
            
            myUsername = user;
            connectWebSocket();
            
            // Ждем установки соединения перед отправкой авторизации
            ws.onopen = () => {
                ws.send(JSON.stringify({ type: "login", username: user, password: pass }));
            };
        }

        function sendMessage() {
            const input = document.getElementById('message-input');
            const text = input.value.trim();
            if (!text || ws.readyState !== WebSocket.OPEN) return;
            
            ws.send(JSON.stringify({ type: "chat", text: text }));
            input.value = '';
        }

        function renderMessage(user, text, time) {
            const div = document.createElement('div');
            div.className = 'message ' + (user === myUsername ? 'my' : '');
            div.innerHTML = `<span class="meta"><b>${user}</b> • ${time || ''}</span>${text}`;
            
            const msgBox = document.getElementById('messages');
            msgBox.appendChild(div);
            msgBox.scrollTop = msgBox.scrollHeight; // Автоскролл вниз
        }
    </script>
</body>
</html>
```

---

### Шаг 3: Локальный запуск на Linux

Открой терминал в папке с твоим проектом (где лежит `main.go` и папка `public`) и выполни следующие команды:

1. **Инициализация Go-модуля:**
   ```bash
   go mod init minimessenger
   ```
2. **Скачивание зависимостей:**
   ```bash
   go get github.com/gorilla/websocket
   go get modernc.org/sqlite
   ```
3. **Запуск сервера:**
   ```bash
   go run main.go
   ```

Теперь чат доступен локально по адресу: `http://localhost:8080`. Можешь открыть две разные вкладки браузера, залогиниться под разными именами и проверить обмен сообщениями.

---

### Шаг 4: Вывод в интернет через Cloudflare Tunnel (Без портов и IP)

Cloudflare предоставляет отличную фичу под названием **Quick Tunnels**. Она поднимает зашифрованный туннель от твоего локального демона до серверов Cloudflare и выдает тебе временный (но абсолютно бесплатный) HTTPS домен вида `https://random-words.trycloudflare.com`.

Тебе не нужно трогать роутер или фаервол — соединение исходящее.

1. **Установка `cloudflared` на твой Linux:**
   В новом окне терминала (не закрывая сервер Go) скачай и установи пакет (команда для Debian/Ubuntu-подобных систем, если у тебя Arch/Fedora, используй их пакетный менеджер):
   ```bash
   wget -q https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64.deb
   sudo dpkg -i cloudflared-linux-amd64.deb
   ```

2. **Запуск туннеля:**
   Просто укажи порт, на котором крутится твой Go-сервер (в нашем случае 8080):
   ```bash
   cloudflared tunnel --url http://localhost:8080
   ```

3. **Получение ссылки:**
   В терминале побежит лог. Найди в нем строку, похожую на эту:
   ```text
   INF |  Your quick Tunnel has been created! Visit it at (it may take some time to be reachable):
   INF |  https://some-random-words.trycloudflare.com
   ```

**Всё!** Отправляй этот `https://...` линк друзьям. 
* Во фронтенде я специально использовал конструкцию `const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';`, чтобы при заходе через HTTPS от Cloudflare, вебсокеты автоматически переключались на безопасный `wss://`.

*Примечание по безопасности:* Для друзей это отличное решение. Но учти, что пароли сейчас хранятся в БД в открытом виде (plaintext). Если надумаешь развивать проект и пускать туда посторонних, перед записью в SQLite прогоняй пароли через пакет `golang.org/x/crypto/bcrypt`.