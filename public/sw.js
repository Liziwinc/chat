// public/sw.js

// Слушаем push-события, которые приходят с вашего Go-сервера
self.addEventListener('push', function(event) {
    // Пытаемся прочитать данные, которые пришли с сервера
    const data = event.data ? event.data.json() : { title: 'Новое сообщение', body: 'У вас новое сообщение в чате' };
    
    // Показываем уведомление
    const promiseChain = self.registration.showNotification(data.title, {
        body: data.body,
        icon: '/icon-192x192.png', // Не забудьте добавить иконку в папку public
        badge: '/badge-72x72.png',
        vibrate: [200, 100, 200], // Вибросигнал для мобильных устройств
        data: {
            url: '/' // URL, который откроется при клике на уведомление
        }
    });

    event.waitUntil(promiseChain);
});

// Обрабатываем клик по уведомлению
self.addEventListener('notificationclick', function(event) {
    event.notification.close();

    // Открываем или фокусируем окно с чатом
    event.waitUntil(
        clients.matchAll({ type: 'window' }).then(windowClients => {
            // Если окно с нашим приложением уже открыто, просто фокусируем его
            for (var i = 0; i < windowClients.length; i++) {
                var client = windowClients[i];
                if (client.url === '/' && 'focus' in client) {
                    return client.focus();
                }
            }
            // Если нет — открываем новое
            if (clients.openWindow) {
                return clients.openWindow('/');
            }
        })
    );
});