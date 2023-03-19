from flask import *

app = Flask(__name__)
@app.route("/", methods=["POST"])
def generate_text():
    input_text = request.form["message"]
    return input_text + " ですわ"

if __name__ == "__main__":
    app.run(debug=True, host='0.0.0.0', port=3000, threaded=True)