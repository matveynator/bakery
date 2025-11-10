// The admin script manages inventory CRUD actions via the JSON API.
function showMessage(text) {
    const box = document.getElementById('admin-message');
    if (!box) return;
    box.textContent = text;
    box.style.display = text ? 'block' : 'none';
    if (text) {
        setTimeout(() => {
            box.style.display = 'none';
        }, 3500);
    }
}

function toRFC3339(value) {
    if (!value) {
        return new Date().toISOString();
    }
    const date = new Date(value);
    return date.toISOString();
}

function fetchInventory() {
    fetch('/api/admin/inventory')
        .then((res) => {
            if (!res.ok) {
                throw new Error('Не удалось получить список');
            }
            return res.json();
        })
        .then(renderRows)
        .catch((err) => {
            showMessage(err.message || 'Ошибка загрузки');
        });
}

function renderRows(items) {
    const tbody = document.getElementById('inventory-rows');
    tbody.innerHTML = '';
    items.forEach((item) => {
        const row = document.createElement('tr');
        row.innerHTML = `
            <td>${item.Name || item.name}</td>
            <td>${item.Category || item.category}</td>
            <td><input type="number" min="0" value="${item.AvailableCount || item.available_count}" data-field="count"></td>
            <td><input type="number" min="0" value="${item.PriceCents || item.price_cents}" data-field="price"></td>
            <td><input type="datetime-local" value="${formatLocalTime(item.BakedAt || item.baked_at)}" data-field="baked"></td>
            <td>
                <button type="button" data-action="update">Обновить</button>
                <button type="button" data-action="delete">Удалить</button>
            </td>
        `;
        row.dataset.id = item.ID || item.id;
        tbody.appendChild(row);
    });
}

function formatLocalTime(value) {
    if (!value) return '';
    const date = new Date(value);
    const pad = (num) => String(num).padStart(2, '0');
    const yyyy = date.getFullYear();
    const mm = pad(date.getMonth() + 1);
    const dd = pad(date.getDate());
    const hh = pad(date.getHours());
    const min = pad(date.getMinutes());
    return `${yyyy}-${mm}-${dd}T${hh}:${min}`;
}

function handleTableClick(event) {
    const action = event.target.getAttribute('data-action');
    if (!action) return;
    const row = event.target.closest('tr');
    if (!row) return;
    const id = parseInt(row.dataset.id, 10);
    if (action === 'delete') {
        fetch(`/api/admin/inventory?id=${id}`, { method: 'DELETE' })
            .then((res) => {
                if (!res.ok) {
                    throw new Error('Не удалось удалить партию');
                }
                fetchInventory();
                showMessage('Партия удалена');
            })
            .catch((err) => showMessage(err.message || 'Ошибка удаления'));
        return;
    }
    if (action === 'update') {
        const count = parseInt(row.querySelector('[data-field="count"]').value, 10) || 0;
        const price = parseInt(row.querySelector('[data-field="price"]').value, 10) || 0;
        const baked = row.querySelector('[data-field="baked"]').value;
        fetch('/api/admin/inventory', {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
                id,
                available_count: count,
                price_cents: price,
                baked_at: toRFC3339(baked),
            }),
        })
            .then((res) => {
                if (!res.ok) {
                    throw new Error('Не удалось обновить партию');
                }
                showMessage('Изменения сохранены');
            })
            .catch((err) => showMessage(err.message || 'Ошибка обновления'));
    }
}

function handleFormSubmit(event) {
    event.preventDefault();
    const form = event.target;
    const payload = {
        name: form.elements.name.value,
        category: form.elements.category.value,
        available_count: parseInt(form.elements.available.value, 10) || 0,
        price_cents: parseInt(form.elements.price.value, 10) || 0,
        baked_at: toRFC3339(form.elements.baked.value),
    };

    fetch('/api/admin/inventory', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
    })
        .then((res) => {
            if (!res.ok) {
                return res.text().then((text) => {
                    throw new Error(text || 'Не удалось сохранить партию');
                });
            }
            return res.json();
        })
        .then(() => {
            form.reset();
            showMessage('Партия добавлена');
            fetchInventory();
        })
        .catch((err) => showMessage(err.message || 'Ошибка сохранения'));
}

window.addEventListener('DOMContentLoaded', () => {
    fetchInventory();
    document.getElementById('inventory-form').addEventListener('submit', handleFormSubmit);
    document.getElementById('inventory-rows').addEventListener('click', handleTableClick);
});
