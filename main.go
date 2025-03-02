package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ContainerInfo структура для хранения информации о контейнере
type ContainerInfo struct {
	ID       string
	Labels   map[string]string
	LastSeen time.Time
}

var (
	// Реестр контейнеров для отслеживания их состояния
	containerRegistry = make(map[string]*ContainerInfo)
	registryMutex     = &sync.Mutex{}

	// Метрики Prometheus
	startTimeSecondsContainer = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "start_time_seconds_container",
			Help: "Start time of Docker containers in Unix timestamp",
		},
		[]string{
			"container_label_com_docker_compose_config_hash",
			"container_label_com_docker_compose_container_number",
			"container_label_com_docker_compose_depends_on",
			"container_label_com_docker_compose_image",
			"container_label_com_docker_compose_oneoff",
			"container_label_com_docker_compose_project",
			"container_label_com_docker_compose_project_config_files",
			"container_label_com_docker_compose_project_working_dir",
			"container_label_com_docker_compose_service",
			"container_label_com_docker_compose_version",
			"id",
			"image",
			"name",
		},
	)

	serverHostname = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "server_hostname",
			Help: "Hostname of the server",
		},
		[]string{"hostname"},
	)
)

func init() {
	// Регистрация метрик в Prometheus
	prometheus.MustRegister(startTimeSecondsContainer)
	prometheus.MustRegister(serverHostname)
}

// Получение имени хоста Docker-сервера
func getHostHostname(ctx context.Context, cli *client.Client) (string, error) {
	info, err := cli.Info(ctx)
	if err != nil {
		return "unknown", fmt.Errorf("ошибка при получении hostname хоста: %w", err)
	}
	return info.Name, nil
}

// Получение информации о контейнерах
func getContainerInfo(ctx context.Context, cli *client.Client) (map[string]map[string]interface{}, error) {
	containers, err := cli.ContainerList(ctx, types.ContainerListOptions{})
	if err != nil {
		return nil, fmt.Errorf("ошибка при получении списка контейнеров: %w", err)
	}

	containerInfo := make(map[string]map[string]interface{})
	currentTime := time.Now()
	registryMutex.Lock()
	defer registryMutex.Unlock()

	for _, container := range containers {
		inspect, err := cli.ContainerInspect(ctx, container.ID)
		if err != nil {
			log.Printf("Ошибка при инспектировании контейнера %s: %v", container.ID, err)
			continue
		}

		name := strings.TrimPrefix(inspect.Name, "/")
		startTimeStr := inspect.State.StartedAt
		startTime, err := time.Parse(time.RFC3339Nano, startTimeStr)
		if err != nil {
			log.Printf("Ошибка при парсинге времени запуска контейнера %s: %v", name, err)
			continue
		}

		labels := inspect.Config.Labels
		containerID := inspect.ID

		// Проверяем, не изменился ли ID контейнера
		if oldInfo, exists := containerRegistry[name]; exists {
			if oldInfo.ID != containerID {
				// Контейнер был перезапущен - удаляем старую метрику
				log.Printf("Обнаружен перезапуск контейнера %s, очищаем старые метрики", name)
				clearContainerMetrics(name, oldInfo.Labels)
			}
		}

		// Формируем метки для метрик
		metricLabels := map[string]string{
			"container_label_com_docker_compose_config_hash":          labels["com.docker.compose.config-hash"],
			"container_label_com_docker_compose_container_number":     labels["com.docker.compose.container-number"],
			"container_label_com_docker_compose_depends_on":           labels["com.docker.compose.depends_on"],
			"container_label_com_docker_compose_image":                labels["com.docker.compose.image"],
			"container_label_com_docker_compose_oneoff":               labels["com.docker.compose.oneoff"],
			"container_label_com_docker_compose_project":              labels["com.docker.compose.project"],
			"container_label_com_docker_compose_project_config_files": labels["com.docker.compose.project.config_files"],
			"container_label_com_docker_compose_project_working_dir":  labels["com.docker.compose.project.working_dir"],
			"container_label_com_docker_compose_service":              labels["com.docker.compose.service"],
			"container_label_com_docker_compose_version":              labels["com.docker.compose.version"],
			"id":                                                      containerID,
			"image":                                                   inspect.Config.Image,
			"name":                                                    name,
		}

		// Обновляем реестр контейнеров
		containerRegistry[name] = &ContainerInfo{
			ID:       containerID,
			Labels:   metricLabels,
			LastSeen: currentTime,
		}

		containerInfo[name] = map[string]interface{}{
			"start_time_unix": startTime.Unix(),
			"labels":          metricLabels,
		}
	}

	return containerInfo, nil
}

// Очистка метрик для конкретного контейнера
func clearContainerMetrics(containerName string, labels map[string]string) {
	startTimeSecondsContainer.Delete(labels)
	log.Printf("Очищены метрики для контейнера: %s", containerName)
}

// Очистка метрик для устаревших контейнеров
func cleanupStaleContainers(currentTime time.Time, timeout int) {
	var staleContainers []string

	for name, info := range containerRegistry {
		if currentTime.Sub(info.LastSeen).Seconds() > float64(timeout) {
			staleContainers = append(staleContainers, name)
			clearContainerMetrics(name, info.Labels)
		}
	}

	// Удаляем устаревшие записи из реестра
	for _, name := range staleContainers {
		delete(containerRegistry, name)
		log.Printf("Удален устаревший контейнер из реестра: %s", name)
	}
}

// Обновление метрик Prometheus
func updateMetrics(ctx context.Context, cli *client.Client) error {
	// Получаем информацию о контейнерах
	containerInfo, err := getContainerInfo(ctx, cli)
	if err != nil {
		return err
	}

	// Обновляем метрики контейнеров
	for _, info := range containerInfo {
		labels := info["labels"].(map[string]string)
		startTimeUnix := info["start_time_unix"].(int64)
		startTimeSecondsContainer.With(prometheus.Labels(labels)).Set(float64(startTimeUnix))
	}

	// Очищаем метрики контейнеров, которые не были обновлены
	registryMutex.Lock()
	cleanupStaleContainers(time.Now(), 30)
	registryMutex.Unlock()

	// Обновляем метрику имени хоста
	hostname, err := getHostHostname(ctx, cli)
	if err != nil {
		log.Printf("Ошибка при получении имени хоста: %v", err)
		hostname = "unknown"
	}
	serverHostname.With(prometheus.Labels{"hostname": hostname}).Set(1)
	return nil
}

// Обработка событий Docker для отслеживания изменений контейнеров
func monitorDockerEvents(ctx context.Context, cli *client.Client) {
	// Устанавливаем фильтр для событий контейнеров
	filters := filters.NewArgs()
	filters.Add("type", "container")
	
	// Подписываемся на события Docker
	options := types.EventsOptions{
		Filters: filters,
	}
	eventsChan, errChan := cli.Events(ctx, options)

	for {
		select {
		case event := <-eventsChan:
			containerID := event.Actor.ID
			containerName := strings.TrimPrefix(event.Actor.Attributes["name"], "/")
			
			// Обрабатываем события связанные с изменением состояния контейнера
			switch event.Action {
			case "start":
				log.Printf("Событие: контейнер %s (%s) запущен", containerName, containerID)
				// Обновляем метрики, чтобы добавить новый контейнер
				if err := updateMetrics(ctx, cli); err != nil {
					log.Printf("Ошибка при обновлении метрик после запуска контейнера: %v", err)
				}
				
			case "die", "stop", "kill", "destroy", "remove":
				log.Printf("Событие: контейнер %s (%s) остановлен/удален: %s", containerName, containerID, event.Action)
				registryMutex.Lock()
				if info, exists := containerRegistry[containerName]; exists {
					clearContainerMetrics(containerName, info.Labels)
					delete(containerRegistry, containerName)
					log.Printf("Удалены метрики для контейнера %s", containerName)
				}
				registryMutex.Unlock()
				
			case "restart":
				log.Printf("Событие: контейнер %s (%s) перезапущен", containerName, containerID)
				registryMutex.Lock()
				if info, exists := containerRegistry[containerName]; exists {
					clearContainerMetrics(containerName, info.Labels)
					delete(containerRegistry, containerName)
					log.Printf("Удалены метрики для перезапущенного контейнера %s", containerName)
				}
				registryMutex.Unlock()
				// Обновляем метрики, чтобы добавить перезапущенный контейнер
				if err := updateMetrics(ctx, cli); err != nil {
					log.Printf("Ошибка при обновлении метрик после перезапуска контейнера: %v", err)
				}
			}
			
		case err := <-errChan:
			if err != nil {
				log.Printf("Ошибка при мониторинге событий Docker: %v", err)
				// Пытаемся восстановить мониторинг после короткой паузы
				time.Sleep(5 * time.Second)
				return
			}
			
		case <-ctx.Done():
			log.Println("Остановка мониторинга событий Docker")
			return
		}
	}
}

func main() {
	// Получение порта из переменной окружения
	port := 8000
	if portEnv := os.Getenv("EXPORTER_PORT"); portEnv != "" {
		if p, err := strconv.Atoi(portEnv); err == nil {
			port = p
		}
	}

	// Инициализация клиента Docker
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("Ошибка при создании клиента Docker: %v", err)
	}
	defer cli.Close()

	// Создание контекста с возможностью отмены
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Обработка сигналов для корректного завершения
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	
	// Запуск HTTP-сервера для метрик
	http.Handle("/metrics", promhttp.Handler())
	server := &http.Server{Addr: fmt.Sprintf(":%d", port)}

	go func() {
		log.Printf("Экспортер метрик запущен на порту %d", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Ошибка при запуске HTTP-сервера: %v", err)
		}
	}()

	// Запуск мониторинга событий Docker
	go func() {
		log.Println("Запуск мониторинга событий Docker")
		for {
			monitorDockerEvents(ctx, cli)
			select {
			case <-ctx.Done():
				return
			default:
				log.Println("Перезапуск мониторинга событий Docker")
				time.Sleep(1 * time.Second)
			}
		}
	}()

	// Периодическое обновление метрик
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	go func() {
		// Выполняем первое обновление сразу
		if err := updateMetrics(ctx, cli); err != nil {
			log.Printf("Ошибка при обновлении метрик: %v", err)
		}

		// Затем по тикеру
		for {
			select {
			case <-ticker.C:
				if err := updateMetrics(ctx, cli); err != nil {
					log.Printf("Ошибка при обновлении метрик: %v", err)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Ожидание сигнала для завершения
	<-sigChan
	log.Println("Получен сигнал завершения, закрываем приложение...")
	
	// Корректное завершение работы
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("Ошибка при остановке HTTP-сервера: %v", err)
	}
	
	cancel() // Отменяем контекст для остановки всех горутин
	log.Println("Приложение успешно завершено")
} 
