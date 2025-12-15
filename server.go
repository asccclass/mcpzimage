package main

import (
	"os"
	"fmt"
	"log"
	"sync"
	"time"
	"net/http"
	"os/exec"
	"encoding/json"
	"path/filepath"

	"github.com/asccclass/sherryserver"
	"github.com/gorilla/websocket"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"github.com/joho/godotenv"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// --- 1. 資料庫模型 (SQLite) ---
type Task struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Prompt    string    `json:"prompt"`
	Status    string    `json:"status"` // Pending, Processing, Completed, Failed
	ImagePath string    `json:"image_path"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

var db *gorm.DB

// --- 2. WebSocket 管理 ---
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// 用來管理所有連線的 Clients，以便廣播訊息
var clients = make(map[*websocket.Conn]bool)
var broadcast = make(chan []byte)
var mutex = &sync.Mutex{}

// 前端傳來的訊息格式
type WSMessage struct {
	Type   string `json:"type"`   // "create_task", "get_history"
	Prompt string `json:"prompt"` // 用於 create_task
}

// 回傳給前端的訊息格式
type WSResponse struct {
	Type string      `json:"type"` // "history", "update", "new_task"
	Data interface{} `json:"data"`
}

func handleMessages() {
	for {
		// 從 broadcast channel 收到訊息，推播給所有連線者
		msg := <-broadcast
		mutex.Lock()
		for client := range clients {
			err := client.WriteMessage(websocket.TextMessage, msg)
			if err != nil {
				client.Close()
				delete(clients, client)
			}
		}
		mutex.Unlock()
	}
}

// --- 背景 Worker (Message Queue Consumer) ---
func taskWorker() {
	for {
		var task Task
		found := false

		// 修正點：接收 err 並在下方檢查
		err := db.Transaction(func(tx *gorm.DB) error {
			// 1. 嘗試鎖定並讀取一筆 "Pending" 的任務
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("status = ?", "Pending").
				Order("created_at asc").
				First(&task).Error; err != nil {
				return err
			}

			// 2. 找到任務後，立即在交易內標記為 "Processing"
			task.Status = "Processing"
			if err := tx.Save(&task).Error; err != nil {
				return err
			}
			found = true
			return nil
		})

		// 修正點：這裡加入對 err 的檢查 (雖然主要邏輯依賴 found，但印出錯誤有助於除錯)
		if err != nil && err != gorm.ErrRecordNotFound {
			log.Printf("Database transaction error: %v", err)
		}

		if found {
			// --- 交易已提交，鎖已釋放 ---
			
			// 通知前端
			notifyUpdate(task)

			// 3. 執行 Python 生成
			log.Printf("Processing Task ID %d: %s", task.ID, task.Prompt)
			imagePath, genErr := runPythonZImage(task.Prompt, task.ID) // 注意變數名稱避免衝突

			// 4. 更新最終結果
			if genErr != nil {
				task.Status = "Failed"
				log.Printf("Task %d failed: %v", task.ID, genErr)
			} else {
				task.Status = "Completed"
				task.ImagePath = imagePath
				log.Printf("Task %d completed", task.ID)
			}
			db.Save(&task)
			notifyUpdate(task)

		} else {
			// 沒有任務，休息一下
			time.Sleep(2 * time.Second)
		}
	}
}

func notifyUpdate(task Task) {
	resp := WSResponse{Type: "update", Data: task}
	jsonResp, _ := json.Marshal(resp)
	broadcast <- jsonResp
}

// 呼叫 Python 腳本
func runPythonZImage(prompt string, id uint) (string, error) {
	// 定義輸出路徑
	fileName := fmt.Sprintf("task_%d_%d.png", id, time.Now().Unix())
	outputDir := os.Getenv("DocumentRoot") + "/images"
	os.MkdirAll(outputDir, os.ModePerm)
	
	// 使用絕對路徑
	absOutputDir, _ := filepath.Abs(outputDir)
	absOutputPath := filepath.Join(absOutputDir, fileName)

	// 設定 Z-Image 專案路徑 (請修改為您的實際路徑)
	zImageProjectDir := "./Z-Image" 
	scriptPath := filepath.Join(zImageProjectDir, "run_z_image.py")

	cmd := exec.Command("python", scriptPath, "--prompt", prompt, "--output", absOutputPath)
	cmd.Dir = zImageProjectDir // 設定工作目錄

	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("python error: %v, log: %s", err, string(output))
	}
	return fileName, nil // 回傳檔案名稱給前端使用
}


// --- WebSocket 處理邏輯 ---
func serveWs(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	defer ws.Close()

	// 註冊連線
	mutex.Lock()
	clients[ws] = true
	mutex.Unlock()

	for {
		var msg WSMessage
		// 讀取 JSON 訊息
		err := ws.ReadJSON(&msg)
		if err != nil {
			mutex.Lock()
			delete(clients, ws)
			mutex.Unlock()
			break
		}

		if msg.Type == "get_history" {
			// 讀取最近 20 筆任務
			var tasks []Task
			db.Order("created_at desc").Limit(20).Find(&tasks)
			resp := WSResponse{Type: "history", Data: tasks}
			ws.WriteJSON(resp)

		} else if msg.Type == "create_task" {
			// 建立新任務 (寫入 SQLite)
			newTask := Task{
				Prompt: msg.Prompt,
				Status: "Pending",
			}
			db.Create(&newTask)

			// 通知所有前端有新任務
			resp := WSResponse{Type: "new_task", Data: newTask}
			jsonResp, _ := json.Marshal(resp)
			broadcast <- jsonResp
		}
	}
}

func main() {
   if err := godotenv.Load("envfile"); err != nil {
      fmt.Println(err.Error())
      return
   }
	// 初始化 SQLite
	var err error
	db, err = gorm.Open(sqlite.Open(os.Getenv("DBPath") +"queue.db"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent), // 設定為靜音模式
	})
	if err != nil {
		log.Fatal("failed to connect database", err)
	}
	// 自動建立資料表
	db.AutoMigrate(&Task{})

	// 啟動背景 Worker (處理佇列)
	go taskWorker()

	// 啟動 WebSocket 廣播監聽器
	go handleMessages()

	// 初始化 Web Server
   port := os.Getenv("PORT")
   if port == "" {
      port = "80"
   }
   documentRoot := os.Getenv("DocumentRoot")
   if documentRoot == "" {
      documentRoot = "www/html"
   }
   templateRoot := os.Getenv("TemplateRoot")
   if templateRoot == "" {
      templateRoot = "www/template"
   }

   server, err := SherryServer.NewServer(":" + port, documentRoot, templateRoot)
   if err != nil {
      panic(err)
   }
   router := NewRouter(server, documentRoot)
   if router == nil {
      fmt.Println("router return nil")
      return
   }
   server.Server.Handler = router  // server.CheckCROS(router)  // 需要自行implement, overwrite 預設的
   server.Start()
}
