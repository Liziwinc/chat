// public/sw.js

// Слушаем приход push-уведомления от сервера
self.addEventListener('push', function(event) {
    if (event.data) {
        const data = event.data.json();
        
        const options = {
            body: data.body,
            icon: '/icon-192.png', // Убедись, что добавишь иконку приложения
            badge: '/badge.png',   // Маленькая монохромная иконка для шторки уведомлений (опционально)
            vibrate: [200, 100, 200],
            data: { url: '/' }
        };

        // Показываем само уведомление
        event.waitUntil(
            self.registration.showNotification(data.title, options).then(() => {
                // Включаем значок (красную точку) на иконке приложения (App Badging API)
                if ('setAppBadge' in navigator) {
                    navigator.setAppBadge(); // Можно передать число, например navigator.setAppBadge(1)
                }
            })
        );
    }
});

// Что делать при клике на уведомление
self.addEventListener('notificationclick', function(event) {
    event.notification.close(); // Закрываем уведомление

    event.waitUntil(
        clients.matchAll({ type: 'window' }).then(windowClients => {
            // Проверяем, открыта ли уже вкладка с чатом
            for (var i = 0; i < windowClients.length; i++) {
                var client = windowClients[i];
                if (client.url === '/' && 'focus' in client) {
                    return client.focus();
                }
            }
            // Если нет, открываем новую
            if (clients.openWindow) {
                return clients.openWindow('/');
            }
        })
    );
});