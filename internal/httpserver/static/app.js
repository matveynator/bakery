// The front-end script keeps the order summary updated and communicates with the Go backend via fetch.
const state = {
    items: {},
};

const weekdays = ['monday', 'tuesday', 'wednesday', 'thursday', 'friday', 'saturday', 'sunday'];

function renderMenu(menu) {
    const grid = document.querySelector('.menu-grid');
    grid.innerHTML = '';
    menu.forEach((item) => {
        const card = document.createElement('div');
        card.className = 'menu-item';
        card.dataset.item = item.name;
        card.dataset.category = item.category || '';
        card.innerHTML = `
            <img src="${item.image}" alt="${item.name}">
            <div class="content">
                <h3>${item.name}</h3>
                <p>${item.description}</p>
                <div class="price">${item.price}</div>
                <div class="controls">
                    <button type="button" class="decrement">−</button>
                    <span class="counter">0</span>
                    <button type="button" class="increment">+</button>
                </div>
            </div>
        `;
        grid.appendChild(card);
    });
    attachMenuHandlers();
}

function attachMenuHandlers() {
    state.items = {};
    document.querySelectorAll('[data-item]').forEach((card) => {
        const name = card.getAttribute('data-item');
        state.items[name] = 0;
        card.querySelector('.increment').addEventListener('click', () => changeCount(name, 1));
        card.querySelector('.decrement').addEventListener('click', () => changeCount(name, -1));
    });
    updateSummary();
}

function updateSummary() {
    const list = document.querySelector('#order-summary');
    list.innerHTML = '';
    const entries = Object.entries(state.items).filter(([, qty]) => qty > 0);
    if (entries.length === 0) {
        const empty = document.createElement('li');
        empty.textContent = 'Корзина пока пустая';
        list.appendChild(empty);
        return;
    }
    entries.forEach(([name, qty]) => {
        const item = document.createElement('li');
        const label = document.createElement('span');
        label.textContent = name;
        const quantity = document.createElement('span');
        quantity.textContent = `${qty} шт.`;
        item.appendChild(label);
        item.appendChild(quantity);
        list.appendChild(item);
    });
}

function changeCount(name, delta) {
    const current = state.items[name] || 0;
    const next = Math.max(0, current + delta);
    state.items[name] = next;
    const counter = document.querySelector(`[data-item="${name}"] .counter`);
    if (counter) {
        counter.textContent = next;
    }
    updateSummary();
}

function gatherBreadDays() {
    const selected = [];
    document.querySelectorAll('.weekday-grid input:checked').forEach((checkbox) => {
        selected.push(checkbox.value);
    });
    return selected;
}

function gatherCroissantSchedule() {
    const plan = [];
    document.querySelectorAll('#croissant-days .croissant-row').forEach((row) => {
        const day = row.getAttribute('data-day');
        const value = parseInt(row.querySelector('input').value, 10) || 0;
        if (value > 0) {
            plan.push({ day, quantity: value });
        }
    });
    return plan;
}

function resetCounters() {
    Object.keys(state.items).forEach((key) => {
        state.items[key] = 0;
    });
    document.querySelectorAll('.counter').forEach((el) => (el.textContent = '0'));
    updateSummary();
}

function handleSubmit(event) {
    event.preventDefault();
    const entries = Object.entries(state.items).filter(([, qty]) => qty > 0);
    if (entries.length === 0) {
        notify('Добавьте выпечку в заказ перед отправкой.');
        return;
    }

    const breadDays = gatherBreadDays();
    const croissants = gatherCroissantSchedule();
    const breadStart = document.querySelector('#bread-start').value;
    const breadFrequency = document.querySelector('#bread-frequency').value;

    if (!breadStart) {
        notify('Укажите дату начала доставки хлеба.');
        return;
    }
    if (breadDays.length === 0) {
        notify('Выберите дни недели для доставки хлеба.');
        return;
    }
    if (croissants.length === 0) {
        notify('Укажите дни и количество круассанов.');
        return;
    }

    const payload = {
        name: document.querySelector('#customer-name').value,
        phone: document.querySelector('#customer-phone').value,
        address: document.querySelector('#customer-address').value,
        comment: document.querySelector('#order-comment').value,
        items: entries.map(([name, qty]) => ({ name, quantity: qty })),
        bread_schedule: {
            days: breadDays,
            frequency: breadFrequency,
            start_date: breadStart,
        },
        croissant_schedule: croissants,
    };

    fetch('/api/orders', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
    })
        .then((res) => {
            if (!res.ok) {
                return res.text().then((text) => {
                    throw new Error(text || 'Ошибка при создании заказа');
                });
            }
            return res.json();
        })
        .then((data) => {
            notify(data.message || 'Заказ принят!');
            event.target.reset();
            document.querySelectorAll('.weekday-grid input').forEach((input) => {
                input.checked = false;
            });
            document.querySelectorAll('#croissant-days input').forEach((input) => {
                input.value = 0;
            });
            resetCounters();
        })
        .catch((err) => {
            notify(err.message || 'Не удалось отправить заказ');
        });
}

function notify(message) {
    const box = document.querySelector('#notification');
    box.textContent = message;
    box.style.display = 'block';
    setTimeout(() => {
        box.style.display = 'none';
    }, 4000);
}

function fetchMenu() {
    fetch('/api/menu')
        .then((res) => {
            if (!res.ok) {
                throw new Error('Не удалось обновить меню');
            }
            return res.json();
        })
        .then((menu) => {
            if (Array.isArray(menu) && menu.length > 0) {
                renderMenu(menu.map((m) => ({
                    name: m.Name || m.name,
                    description: m.Description || m.description,
                    price: m.Price || m.price,
                    image: m.Image || m.image,
                    category: m.Category || m.category,
                })));
            } else {
                attachMenuHandlers();
            }
        })
        .catch(() => {
            attachMenuHandlers();
        });
}

window.addEventListener('DOMContentLoaded', () => {
    const templateCards = Array.from(document.querySelectorAll('[data-item]')).map((card) => ({
        name: card.getAttribute('data-item'),
        description: card.querySelector('p').textContent,
        price: card.querySelector('.price').textContent,
        image: card.querySelector('img').getAttribute('src'),
        category: card.getAttribute('data-category') || '',
    }));
    renderMenu(templateCards);
    fetchMenu();

    const form = document.querySelector('#order-form');
    form.addEventListener('submit', handleSubmit);
});
