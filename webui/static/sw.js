const CACHE='readalong-shell-v5';const SHELL=['/','/app.css','/app.js','/reader.html','/reader.js','/manifest.webmanifest','/icons/icon-192.png','/icons/icon-512.png'];
self.addEventListener('install',e=>e.waitUntil(caches.open(CACHE).then(c=>c.addAll(SHELL)).then(()=>self.skipWaiting())));
self.addEventListener('activate',e=>e.waitUntil(caches.keys().then(keys=>Promise.all(keys.filter(k=>k!==CACHE).map(k=>caches.delete(k)))).then(()=>self.clients.claim())));
self.addEventListener('fetch',e=>{const u=new URL(e.request.url);if(e.request.method!=='GET'||u.pathname.startsWith('/api/'))return;if(u.pathname.startsWith('/reader/')){e.respondWith(caches.match('/reader.html').then(x=>x||fetch(e.request)));return}e.respondWith(caches.match(e.request).then(x=>x||fetch(e.request)))})
