body, html {
    font-family: 'Arial', sans-serif;
    background-color: #1a1a1a;
    color: #f4f4f9;
    display: flex;
    justify-content: center;
    align-items: center;
}

body::-webkit-scrollbar {
    display: none;
}

.main-container {
    width: 100%;
    max-width: 1150px;
    display: flex;
    flex-wrap: wrap;
    justify-content: center;
    align-items: center;
}
.container { 
    width: 100%;
    background-color: #262626;
    padding: 20px;
    border-radius: 10px;
    margin: 10px;
    box-shadow: 0 4px 6px rgba(0, 0, 0, 0.4);
}
.container form{
    display: flex;
    flex-wrap: wrap;
    justify-content: center;
    align-items: center;
}
.container article {
    width: 100%;
    max-width: 505px;
    margin: 10px;
    display: flex;
    flex-wrap: wrap;
    justify-content: center;
    align-items: center;
    background-color: #353434;
    padding: 10px;
    border-radius: 10px;
    margin-bottom: 20px;
    box-shadow: 0 4px 6px rgba(0, 0, 0, 0.4);
}

article input{
    padding: 10px;
    margin: 10px;
    border: 1px solid #555;
    border-radius: 5px;
    width: 100%;
    color: #f4f4f9;
    background-color: #1a1a1a;
}

.Start_button {
    margin: 10px;
    width: 80%;
    padding: 12.5px 30px;
    border: 0;
    border-radius: 100px;
    background-color: #2ba8fb;
    color: #ffffff;
    font-weight: Bold;
    transition: all 0.5s;
    -webkit-transition: all 0.5s;
}

.Start_button:hover {
    background-color: #6fc5ff;
    box-shadow: 0 0 20px #6fc5ff50;
    transform: scale(1.1);
}

.Start_button:active {
    background-color: #3d94cf;
    transition: all 0.25s;
    -webkit-transition: all 0.25s;
    box-shadow: none;
    transform: scale(0.98);
}

.topc{
    display: flex;
    text-align: center;
    justify-content: space-between;
}

.topc input{
    max-width: 280px;
    padding: 10px;
    margin: 5px;
    border: 1px solid #555;
    border-radius: 5px;
    width: 100%;
    color: #f4f4f9;
    background-color: #1a1a1a;
}

.add_button {
    cursor: pointer;
    background: rgba(255, 255, 255, 0.15);
    backdrop-filter: blur(10px);
    border: 1px solid rgba(255, 255, 255, 0.2);
    color: #fff;
    padding: 8px 13px;
    border-radius: 0.5rem;
    transition: background 0.3s ease-in-out;
}

.add_button:hover {
    background: rgba(255, 255, 255, 0.25);
}


.scroller {
    max-height: 180px;
    overflow: auto;
}

.scroller::-webkit-scrollbar {
    display: none;
}
  
table {
    width: 100%;
    margin-top: 10px;
}

th, td {
    padding: 8px;
    text-align: left;
    border: 1px solid #555;
}
